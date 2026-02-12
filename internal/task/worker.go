package task

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	qzone "github.com/guohuiyuan/qzone-go"
	"github.com/guohuiyuan/qzonewall-go/internal/config"
	"github.com/guohuiyuan/qzonewall-go/internal/model"
	"github.com/guohuiyuan/qzonewall-go/internal/render"
	"github.com/guohuiyuan/qzonewall-go/internal/rkey"
	"github.com/guohuiyuan/qzonewall-go/internal/store"

	zero "github.com/wdvxdr1123/ZeroBot" // 新增引入
)

// Worker 定时轮询已通过稿件并发布到 QQ 空间。
type Worker struct {
	cfg         config.WorkerConfig
	wallCfg     config.WallConfig
	client      *qzone.Client
	store       *store.Store
	renderer    *render.Renderer
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	lastPublish time.Time
	mu          sync.Mutex
}

// NewWorker creates a worker.
func NewWorker(
	cfg config.WorkerConfig,
	wallCfg config.WallConfig,
	client *qzone.Client,
	st *store.Store,
	renderer *render.Renderer,
) *Worker {
	ctx, cancel := context.WithCancel(context.Background())
	return &Worker{
		cfg:      cfg,
		wallCfg:  wallCfg,
		client:   client,
		store:    st,
		renderer: renderer,
		ctx:      ctx,
		cancel:   cancel,
	}
}

// Start 启动 worker goroutine。
func (w *Worker) Start() {
	for i := 0; i < w.cfg.Workers; i++ {
		w.wg.Add(1)
		go w.run(i)
	}
	log.Printf("[Worker] 启动 %d 个工作协程，轮询间隔=%v", w.cfg.Workers, w.cfg.PollInterval)
}

// Stop 优雅停止。
func (w *Worker) Stop() {
	w.cancel()
	w.wg.Wait()
	log.Println("[Worker] stopped")
}

func (w *Worker) run(id int) {
	defer w.wg.Done()
	log.Printf("[Worker-%d] started polling", id)

	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-w.ctx.Done():
			log.Printf("[Worker-%d] 收到停止信号", id)
			return
		case <-ticker.C:
			w.pollAndPublish(id)
		}
	}
}

func (w *Worker) pollAndPublish(workerID int) {
	// 拉取已通过但未发布的稿件 (tid='')。
	posts, err := w.store.GetApprovedPosts(1)
	if err != nil {
		log.Printf("[Worker-%d] 查询失败: %v", workerID, err)
		return
	}
	if len(posts) == 0 {
		return
	}

	post := posts[0]
	log.Printf("[Worker-%d] 处理稿件 #%d", workerID, post.ID)

	// 频率限制。
	w.waitRateLimit()

	// Publish with retries.
	var lastErr error
	for retry := 0; retry <= w.cfg.RetryCount; retry++ {
		if retry > 0 {
			log.Printf("[Worker-%d] 重试第 %d 次...", workerID, retry)
			time.Sleep(w.cfg.RetryDelay)
		}

		err := w.publish(post)
		if err == nil {
			log.Printf("[Worker-%d] 稿件 #%d 发布成功, tid=%s", workerID, post.ID, post.TID)
			return
		}
		lastErr = err
		log.Printf("[Worker-%d] 发布失败: %v", workerID, err)
	}

	// 所有重试失败后标记为失败。
	post.Status = model.StatusFailed
	post.Reason = fmt.Sprintf("发布失败: %v", lastErr)
	if err := w.store.SavePost(post); err != nil {
		log.Printf("[Worker-%d] 更新状态失败: %v", workerID, err)
	}
	log.Printf("[Worker-%d] 稿件 #%d 最终发布失败: %v", workerID, post.ID, lastErr)
}

// publish 发布到 QQ 空间。
func (w *Worker) publish(post *model.Post) error {
	// 发布前统一校验带 rkey 的图片链接，失效则自动刷新 rkey。
	// 注意：这里只会处理 HTTP 链接，file ID 会被跳过
	w.refreshInvalidRKeyImages(post)

	// 构建说说文本。
	text := post.Text
	if w.wallCfg.ShowAuthor && !post.Anon {
		text = fmt.Sprintf("【来自 %s 的投稿】\n\n%s", post.ShowName(), text)
	}

	// Only publish rendered screenshot, never raw images.
	if !w.renderer.Available() {
		return fmt.Errorf("publish: renderer not available")
	}

	// 渲染前解析 file ID 为 URL
	renderPost := w.resolvePostImages(post)
	screenshot, err := w.renderer.RenderPost(renderPost)
	if err != nil {
		return fmt.Errorf("publish: render screenshot: %w", err)
	}

	opt := &qzone.PublishOption{ImageBytes: [][]byte{screenshot}}

	resp, err := w.client.Publish(w.ctx, text, opt)
	if err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("publish failed: code=%d, msg=%s", resp.Code, resp.Message)
	}

	// 回填 TID。
	if tid := resp.GetString("tid"); tid != "" {
		post.TID = tid
	} else if tid := resp.GetString("t1_tid"); tid != "" {
		post.TID = tid
	} else {
		// Fallback when API does not return a tid.
		post.TID = fmt.Sprintf("published_%d", time.Now().Unix())
	}

	post.Status = model.StatusPublished
	if err := w.store.SavePost(post); err != nil {
		log.Printf("[Worker] 回填 TID 失败: %v", err)
	}

	// 记录发布时间。
	w.mu.Lock()
	w.lastPublish = time.Now()
	w.mu.Unlock()

	return nil
}

func (w *Worker) refreshInvalidRKeyImages(post *model.Post) {
	if len(post.Images) == 0 {
		return
	}

	updated := false
	for i, raw := range post.Images {
		raw = strings.TrimSpace(raw)
		if !hasRKey(raw) {
			continue
		}
		if isImageURLValid(raw) {
			continue
		}

		fixed, err := refreshOneURL(raw)
		if err != nil {
			log.Printf("[Worker] 刷新 rkey 失败: %v | url=%s", err, raw)
			continue
		}

		post.Images[i] = fixed
		updated = true
		log.Printf("[Worker] rkey 已刷新: %s", fixed)
	}

	if updated {
		if err := w.store.SavePost(post); err != nil {
			log.Printf("[Worker] 回写刷新后的图片链接失败: %v", err)
		}
	}
}

// ── Image Resolution Helpers ──

func (w *Worker) resolvePostImages(p *model.Post) *model.Post {
	clone := *p
	clone.Images = make([]string, len(p.Images))
	for i, img := range p.Images {
		clone.Images[i] = w.resolveImageURL(img)
	}
	return &clone
}

func (w *Worker) resolveImageURL(img string) string {
	if strings.HasPrefix(img, "http") {
		return img
	}
	var resolved string
	zero.RangeBot(func(id int64, ctx *zero.Ctx) bool {
		resolved = ctx.GetImage(img).Get("url").String()
		return true
	})
	if resolved != "" {
		return resolved
	}
	return img
}

func isImageURLValid(raw string) bool {
	if raw == "" {
		return false
	}
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return false
	}

	// Read a small chunk to verify this is an actual image body.
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil || len(b) == 0 {
		return false
	}
	_, _, err = image.DecodeConfig(bytes.NewReader(b))
	return err == nil
}

func hasRKey(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.Query().Get("rkey") != ""
}

func replaceRKey(raw, rkey string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("rkey", rkey)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func refreshOneURL(raw string) (string, error) {
	try := func(candidates []string) (string, bool) {
		for _, c := range candidates {
			fixed, err := replaceRKey(raw, c)
			if err != nil {
				continue
			}
			if isImageURLValid(fixed) {
				return fixed, true
			}
		}
		return "", false
	}

	if fixed, ok := try(rkey.CandidatesForURL(raw)); ok {
		return fixed, nil
	}
	_ = rkey.RefreshFromBots()
	if fixed, ok := try(rkey.CandidatesForURL(raw)); ok {
		return fixed, nil
	}
	return "", fmt.Errorf("no valid rkey candidate matched this resource type")
}

// waitRateLimit 等待频率限制窗口。
func (w *Worker) waitRateLimit() {
	w.mu.Lock()
	last := w.lastPublish
	w.mu.Unlock()

	if last.IsZero() {
		return
	}
	elapsed := time.Since(last)
	if elapsed < w.cfg.RateLimit {
		wait := w.cfg.RateLimit - elapsed
		log.Printf("[Worker] 频率限制，等待 %v", wait)
		time.Sleep(wait)
	}
}