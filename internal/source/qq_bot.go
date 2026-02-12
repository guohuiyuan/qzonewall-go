package source

import (
	"context"
	"encoding/base64"
	"fmt"
	"io" // 新增
	"log"
	"net/http" // 新增
	"strconv"
	"strings"
	"time"

	qzone "github.com/guohuiyuan/qzone-go"
	"github.com/guohuiyuan/qzonewall-go/internal/config"
	"github.com/guohuiyuan/qzonewall-go/internal/model"
	"github.com/guohuiyuan/qzonewall-go/internal/render"
	"github.com/guohuiyuan/qzonewall-go/internal/rkey"
	"github.com/guohuiyuan/qzonewall-go/internal/store"

	zero "github.com/wdvxdr1123/ZeroBot"
	"github.com/wdvxdr1123/ZeroBot/driver"
	"github.com/wdvxdr1123/ZeroBot/message"
)

// QQBot 基于 NapCat + ZeroBot 的 QQ 数据源
type QQBot struct {
	botCfg      config.BotConfig
	wallCfg     config.WallConfig
	qzoneCfg    config.QzoneConfig
	store       *store.Store
	renderer    *render.Renderer
	qzClient    *qzone.Client
	censorWords []string
	engine      *zero.Engine
}

// NewQQBot 创建 QQ 机器人
func NewQQBot(
	botCfg config.BotConfig,
	wallCfg config.WallConfig,
	qzoneCfg config.QzoneConfig,
	st *store.Store,
	renderer *render.Renderer,
	qzClient *qzone.Client,
	censorWords []string,
) *QQBot {
	return &QQBot{
		botCfg:      botCfg,
		wallCfg:     wallCfg,
		qzoneCfg:    qzoneCfg,
		store:       st,
		renderer:    renderer,
		qzClient:    qzClient,
		censorWords: censorWords,
	}
}

// SetClient 设置/替换 QQ空间客户端 (用于延迟初始化)
func (b *QQBot) SetClient(client *qzone.Client) {
	b.qzClient = client
}

// Start 启动 ZeroBot 并注册命令
func (b *QQBot) Start() error {
	b.engine = zero.New()
	b.registerCommands()

	drivers := make([]zero.Driver, 0, len(b.botCfg.WS))
	for _, ws := range b.botCfg.WS {
		drivers = append(drivers, driver.NewWebSocketClient(ws.Url, ws.AccessToken))
	}

	zeroCfg := b.botCfg.Zero
	go func() {
		zero.RunAndBlock(&zero.Config{
			NickName:       zeroCfg.NickName,
			CommandPrefix:  zeroCfg.CommandPrefix,
			SuperUsers:     zeroCfg.SuperUsers,
			RingLen:        zeroCfg.RingLen,
			Latency:        time.Duration(zeroCfg.Latency),
			MaxProcessTime: time.Duration(zeroCfg.MaxProcessTime),
			Driver:         drivers,
		}, nil)
	}()
	b.warmupRKeyCache()

	log.Printf("[QQBot] 已连接 NapCat, %d 个 WS 驱动", len(drivers))
	return nil
}

// Stop 停止
func (b *QQBot) Stop() {
	log.Println("[QQBot] 停止")
}

// ──────────────────────────────────────────
// 命令注册
// ──────────────────────────────────────────

func (b *QQBot) registerCommands() {
	// ── 用户命令 ──
	b.engine.OnCommand("投稿").Handle(b.withRKey(func(ctx *zero.Ctx) {
		b.handleContribute(ctx, false)
	}))
	b.engine.OnCommand("匿名投稿").Handle(b.withRKey(func(ctx *zero.Ctx) {
		b.handleContribute(ctx, true)
	}))
	b.engine.OnCommand("撤稿").Handle(b.withRKey(func(ctx *zero.Ctx) {
		b.handleRecall(ctx)
	}))

	// ── 管理员命令 ──
	b.engine.OnCommand("看稿", zero.SuperUserPermission).SetBlock(true).Handle(b.withRKey(func(ctx *zero.Ctx) {
		b.handleViewPost(ctx)
	}))
	b.engine.OnCommand("过稿", zero.SuperUserPermission).SetBlock(true).Handle(b.withRKey(func(ctx *zero.Ctx) {
		b.handleApprove(ctx)
	}))
	b.engine.OnCommand("拒稿", zero.SuperUserPermission).SetBlock(true).Handle(b.withRKey(func(ctx *zero.Ctx) {
		b.handleReject(ctx)
	}))
	b.engine.OnCommand("待审核", zero.SuperUserPermission).SetBlock(true).Handle(b.withRKey(func(ctx *zero.Ctx) {
		b.handleListPending(ctx)
	}))
	b.engine.OnCommand("发说说", zero.SuperUserPermission).SetBlock(true).Handle(b.withRKey(func(ctx *zero.Ctx) {
		b.handleDirectPublish(ctx)
	}))
	b.engine.OnCommand("扫码", zero.SuperUserPermission).SetBlock(true).Handle(b.withRKey(func(ctx *zero.Ctx) {
		b.handleScanQR(ctx)
	}))
	b.engine.OnCommand("刷新cookie", zero.SuperUserPermission).SetBlock(true).Handle(b.withRKey(func(ctx *zero.Ctx) {
		b.handleRefreshCookie(ctx)
	}))
	b.engine.OnCommandGroup([]string{"帮助", "help"}).SetBlock(true).Handle(b.withRKey(func(ctx *zero.Ctx) {
		b.handleHelp(ctx)
	}))
}

func (b *QQBot) withRKey(next func(*zero.Ctx)) func(*zero.Ctx) {
	return func(ctx *zero.Ctx) {
		b.captureRKey(ctx)
		next(ctx)
	}
}

func (b *QQBot) captureRKey(ctx *zero.Ctx) {
	if ctx == nil {
		return
	}
	_, _ = rkey.UpdateFromRaw(ctx.NcGetRKey().Raw)
}

func (b *QQBot) warmupRKeyCache() {
	go func() {
		// 启动后主动预热 rkey，降低 web/worker 首次刷新时缓存为空的概率。
		for i := 0; i < 60; i++ {
			if strings.TrimSpace(rkey.Get()) != "" {
				return
			}
			if strings.TrimSpace(rkey.RefreshFromBots()) != "" {
				log.Println("[QQBot] rkey cache warmed")
				return
			}
			time.Sleep(time.Second)
		}
		log.Println("[QQBot] rkey cache warmup timeout: no active bot context")
	}()
}

// ──────────────────────────────────────────
// 命令处理逻辑
// ──────────────────────────────────────────

// handleContribute 投稿 / 匿名投稿
func (b *QQBot) handleContribute(ctx *zero.Ctx, anon bool) {
	rawText := getArgs(ctx)
	text := strings.TrimSpace(rawText)
	images := extractImages(ctx)

	if text == "" && len(images) == 0 {
		ctx.Send(message.Text("❌ 投稿内容不能为空，请发送文字或图片"))
		return
	}
	if b.wallCfg.MaxTextLen > 0 && len([]rune(text)) > b.wallCfg.MaxTextLen {
		ctx.Send(message.Text(fmt.Sprintf("❌ 文字超出限制 (%d/%d)", len([]rune(text)), b.wallCfg.MaxTextLen)))
		return
	}
	if b.wallCfg.MaxImages > 0 && len(images) > b.wallCfg.MaxImages {
		ctx.Send(message.Text(fmt.Sprintf("❌ 图片超出限制 (%d/%d)", len(images), b.wallCfg.MaxImages)))
		return
	}

	if len(b.censorWords) > 0 {
		if hit, word := store.CheckCensor(text, b.censorWords); hit {
			ctx.Send(message.Text(fmt.Sprintf("❌ 投稿包含违禁词: %s", word)))
			return
		}
	}

	post := &model.Post{
		UIN:        ctx.Event.UserID,
		Name:       ctx.Event.Sender.NickName,
		GroupID:    ctx.Event.GroupID,
		Text:       text,
		Images:     images,
		Anon:       anon,
		Status:     model.StatusPending,
		CreateTime: time.Now().Unix(),
	}
	if err := b.store.SavePost(post); err != nil {
		ctx.Send(message.Text("❌ 保存失败: " + err.Error()))
		return
	}

	ctx.Send(message.Text(fmt.Sprintf("✅ 投稿成功！编号 #%d，等待审核...", post.ID)))

	if b.botCfg.ManageGroup > 0 {
		notifyMsg := fmt.Sprintf("📬 收到新投稿 #%d\n%s", post.ID, post.Summary())
		ctx.SendGroupMessage(b.botCfg.ManageGroup, message.Text(notifyMsg))
	}
}

// handleRecall 撤稿
func (b *QQBot) handleRecall(ctx *zero.Ctx) {
	args := getArgs(ctx)
	if args == "" {
		ctx.Send(message.Text("用法: /撤稿 <编号>"))
		return
	}
	id, err := strconv.ParseInt(args, 10, 64)
	if err != nil {
		ctx.Send(message.Text("❌ 编号格式不正确"))
		return
	}
	post, err := b.store.GetPost(id)
	if err != nil || post == nil {
		ctx.Send(message.Text(fmt.Sprintf("❌ 稿件 #%d 不存在", id)))
		return
	}
	if post.UIN != ctx.Event.UserID && !zero.SuperUserPermission(ctx) {
		ctx.Send(message.Text("❌ 你只能撤回自己的稿件"))
		return
	}
	if post.Status == model.StatusPublished {
		ctx.Send(message.Text("❌ 已发布的稿件无法撤回"))
		return
	}

	if err := b.store.DeletePost(id); err != nil {
		ctx.Send(message.Text("❌ 撤回失败: " + err.Error()))
		return
	}
	ctx.Send(message.Text(fmt.Sprintf("✅ 稿件 #%d 已撤回", id)))
}

// handleViewPost 看稿
func (b *QQBot) handleViewPost(ctx *zero.Ctx) {
	args := getArgs(ctx)
	if args == "" {
		ctx.Send(message.Text("用法: /看稿 <编号>"))
		return
	}
	id, err := strconv.ParseInt(args, 10, 64)
	if err != nil {
		ctx.Send(message.Text("❌ 编号格式不正确"))
		return
	}
	post, err := b.store.GetPost(id)
	if err != nil || post == nil {
		ctx.Send(message.Text(fmt.Sprintf("❌ 稿件 #%d 不存在", id)))
		return
	}

	if b.renderer.Available() {
		if imgData, err := b.renderer.RenderPost(post); err == nil {
			b64 := base64.StdEncoding.EncodeToString(imgData)
			ctx.Send(message.Image("base64://" + b64))
			return
		} else {
			ctx.Send(message.Text("❌ 渲染失败: " + err.Error()))
		}
	}

	// 降级: 发送原文本+原图
	// 修正：使用 message.Message 类型，而不是 []message.MessageSegment
	var segs message.Message
	segs = append(segs, message.Text(fmt.Sprintf("Post #%d\n%s", post.ID, post.Text)))
	for _, img := range post.Images {
		segs = append(segs, message.Image(img))
	}
	// 修正：ctx.Send 不使用 ... 展开
	ctx.Send(segs)
}

// handleApprove 过稿
func (b *QQBot) handleApprove(ctx *zero.Ctx) {
	args := getArgs(ctx)
	ids, err := parseIDs(args)
	if err != nil {
		ctx.Send(message.Text("❌ " + err.Error() + "\n用法: /过稿 1-4 或 /过稿 1,2,5"))
		return
	}

	posts, err := b.store.GetPostsByIDs(ids)
	if err != nil {
		ctx.Send(message.Text("❌ 数据库查询失败: " + err.Error()))
		return
	}

	var validPosts []*model.Post
	for _, p := range posts {
		if p.Status == model.StatusPending {
			validPosts = append(validPosts, p)
		}
	}

	if len(validPosts) == 0 {
		ctx.Send(message.Text("⚠️ 没有找到[待审核]的稿件，可能已处理"))
		return
	}

	ctx.Send(message.Text(fmt.Sprintf("⏳ 正在处理 %d 条稿件，合并发布中...", len(validPosts))))

	var summaryBuilder strings.Builder
	summaryBuilder.WriteString(fmt.Sprintf("【表白墙更新】 %s\n", time.Now().Format("01/02")))
	summaryBuilder.WriteString("----------------\n")

	// 收集图片数据
	var imagesData [][]byte

	for _, post := range validPosts {
		// A. 渲染图片
		var imgData []byte
		var renderErr error

		if b.renderer.Available() {
			imgData, renderErr = b.renderer.RenderPost(post)
		}

		if renderErr != nil || imgData == nil {
			log.Printf("渲染失败 #%d: %v", post.ID, renderErr)
			ctx.Send(message.Text(fmt.Sprintf("❌ 稿件 #%d 渲染失败，跳过", post.ID)))
			continue
		}

		imagesData = append(imagesData, imgData)

		// B. 拼接摘要
		content := []rune(post.Text)
		if len(content) > 20 {
			summaryBuilder.WriteString(fmt.Sprintf("#%d: %s...\n", post.ID, string(content[:20])))
		} else {
			if post.Text == "" {
				summaryBuilder.WriteString(fmt.Sprintf("#%d: [图片]\n", post.ID))
			} else {
				summaryBuilder.WriteString(fmt.Sprintf("#%d: %s\n", post.ID, post.Text))
			}
		}

		// C. 标记为已发布
		post.Status = model.StatusPublished
		if err := b.store.SavePost(post); err != nil {
			log.Printf("保存稿件状态失败 #%d: %v", post.ID, err)
		}
	}

	if len(imagesData) == 0 {
		ctx.Send(message.Text("❌ 没有成功渲染的图片，取消发布"))
		return
	}

	summaryBuilder.WriteString("----------------\n")
	summaryBuilder.WriteString("详情见图 👇")
	finalText := summaryBuilder.String()

	go func() {
		// 修正：直接使用 ImageBytes 字段，让 qzone 库处理上传逻辑
		opts := &qzone.PublishOption{
			ImageBytes: imagesData,
		}

		// 调用发布接口
		_, publishErr := b.qzClient.Publish(context.Background(), finalText, opts)

		if publishErr != nil {
			log.Printf("发布说说失败: %v", publishErr)
			ctx.Send(message.Text("❌ 发布到空间失败: " + publishErr.Error()))

			// 失败回滚
			for _, p := range validPosts {
				p.Status = model.StatusPending
				if err := b.store.SavePost(p); err != nil {
					log.Printf("回滚稿件状态失败 #%d: %v", p.ID, err)
				}
			}
			return
		}

		// 发布成功：群内反馈
		var msgSegments message.Message
		msgSegments = append(msgSegments, message.Text("✅ 批量过稿成功！已发布到空间：\n"+finalText))

		for _, img := range imagesData {
			b64 := base64.StdEncoding.EncodeToString(img)
			msgSegments = append(msgSegments, message.Image("base64://"+b64))
		}
		ctx.Send(msgSegments)

		// 通知投稿者
		for _, p := range validPosts {
			if p.UIN > 0 {
				notifyMsg := fmt.Sprintf("🎉 您的投稿 #%d 已发布！", p.ID)
				time.Sleep(500 * time.Millisecond)
				if p.GroupID > 0 {
					ctx.SendGroupMessage(p.GroupID, message.Text(notifyMsg))
				} else {
					ctx.SendPrivateMessage(p.UIN, message.Text(notifyMsg))
				}
			}
		}
	}()
}

// handleReject 拒稿
func (b *QQBot) handleReject(ctx *zero.Ctx) {
	argsStr := getArgs(ctx)
	args := strings.Fields(argsStr)
	if len(args) < 1 {
		ctx.Send(message.Text("用法: /拒稿 <编号> [理由]"))
		return
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		ctx.Send(message.Text("❌ 编号格式不正确"))
		return
	}
	post, err := b.store.GetPost(id)
	if err != nil || post == nil {
		ctx.Send(message.Text(fmt.Sprintf("❌ 稿件 #%d 不存在", id)))
		return
	}
	if post.Status == model.StatusPublished {
		ctx.Send(message.Text(fmt.Sprintf("稿件 #%d 已发布，无法拒绝", id)))
		return
	}

	reason := ""
	if len(args) > 1 {
		reason = strings.Join(args[1:], " ")
	}

	post.Status = model.StatusRejected
	post.Reason = reason
	if err := b.store.SavePost(post); err != nil {
		ctx.Send(message.Text("❌ 更新稿件状态失败: " + err.Error()))
		return
	}

	msg := fmt.Sprintf("❌ 稿件 #%d 已拒绝", id)
	if reason != "" {
		msg += "\n理由: " + reason
	}
	ctx.Send(message.Text(msg))

	if post.UIN > 0 {
		notifyMsg := fmt.Sprintf("😔 您的投稿 #%d 未通过审核", post.ID)
		if reason != "" {
			notifyMsg += "\n理由: " + reason
		}
		if post.GroupID > 0 {
			ctx.SendGroupMessage(post.GroupID, message.Text(notifyMsg))
		} else {
			ctx.SendPrivateMessage(post.UIN, message.Text(notifyMsg))
		}
	}
}

// handleListPending 待审核列表
func (b *QQBot) handleListPending(ctx *zero.Ctx) {
	posts, err := b.store.ListByStatus(model.StatusPending)
	if err != nil {
		ctx.Send(message.Text("❌ 查询失败: " + err.Error()))
		return
	}
	if len(posts) == 0 {
		ctx.Send(message.Text("📭 暂无待审核稿件"))
		return
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📋 待审核稿件 (%d 件):\n\n", len(posts)))
	for _, p := range posts {
		sb.WriteString(p.Summary())
		sb.WriteString("---\n")
	}
	ctx.Send(message.Text(sb.String()))
}

// handleDirectPublish 管理员直接发说说
func (b *QQBot) handleDirectPublish(ctx *zero.Ctx) {
	text := getArgs(ctx)
	images := extractImages(ctx) // 1. 提取图片 URL

	if text == "" && len(images) == 0 {
		ctx.Send(message.Text("❌ 内容不能为空"))
		return
	}

	go func() {
		var imagesData [][]byte

		// 2. 如果有图片，需要先下载转为 []byte
		if len(images) > 0 {
			client := &http.Client{Timeout: 20 * time.Second}
			for _, imgURL := range images {
				resp, err := client.Get(imgURL)
				if err != nil {
					log.Printf("[QQBot] 图片下载失败: %v", err)
					continue
				}
				data, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil {
					log.Printf("[QQBot] 图片读取失败: %v", err)
					continue
				}
				if len(data) > 0 {
					imagesData = append(imagesData, data)
				}
			}
		}

		// 3. 构建发布选项
		var opts *qzone.PublishOption
		if len(imagesData) > 0 {
			opts = &qzone.PublishOption{
				ImageBytes: imagesData,
			}
		}

		// 4. 调用发布 (传入 opts)
		_, err := b.qzClient.Publish(context.Background(), text, opts)
		if err != nil {
			ctx.Send(message.Text("❌ 发布失败: " + err.Error()))
		} else {
			ctx.Send(message.Text("✅ 说说已发布"))
		}
	}()
}

// handleScanQR 扫码登录QQ空间
func (b *QQBot) handleScanQR(ctx *zero.Ctx) {
	ctx.Send(message.Text("🔄 正在获取二维码..."))

	qr, err := qzone.GetQRCode()
	if err != nil {
		ctx.Send(message.Text("❌ 获取二维码失败: " + err.Error()))
		return
	}

	b64 := base64.StdEncoding.EncodeToString(qr.Image)
	ctx.Send(message.Image("base64://" + b64))
	ctx.Send(message.Text("📱 请用QQ扫描上方二维码登录QQ空间\n（二维码有效期约2分钟）"))

	go func() {
		for i := 0; i < 60; i++ {
			time.Sleep(2 * time.Second)
			state, cookie, pollErr := qzone.PollQRLogin(qr)
			if pollErr != nil {
				continue
			}
			if state == qzone.LoginSuccess {
				if updateErr := b.qzClient.UpdateCookie(cookie); updateErr != nil {
					ctx.Send(message.Text("❌ Cookie更新失败: " + updateErr.Error()))
					return
				}
				ctx.Send(message.Text(fmt.Sprintf("✅ QQ空间登录成功！UIN=%d", b.qzClient.UIN())))
				return
			}
			if state == qzone.LoginExpired {
				ctx.Send(message.Text("❌ 二维码已过期"))
				return
			}
		}
		ctx.Send(message.Text("❌ 扫码登录超时"))
	}()
}

// handleRefreshCookie
func (b *QQBot) handleRefreshCookie(ctx *zero.Ctx) {
	ctx.Send(message.Text("⚠️ 暂不支持自动刷新，请使用 /扫码 手动登录"))
}

// handleHelp
func (b *QQBot) handleHelp(ctx *zero.Ctx) {
	help := `📖 表白墙Bot使用指南

【投稿命令】
/投稿 <内容>       - 投稿（可附带图片）
/匿名投稿 <内容>   - 匿名投稿
/撤稿 <编号>       - 撤回自己的稿件

【管理命令】（仅管理员）
/待审核             - 查看待审核稿件
/看稿 <编号>        - 查看稿件详情（截图）
/过稿 <编号>        - 通过并发布
/过稿 1-4           - 批量通过 #1~#4
/拒稿 <编号> [理由]  - 拒绝稿件
/发说说 <内容>      - 直接发布到空间
/扫码               - 扫码登录QQ空间`
	ctx.Send(message.Text(help))
}

// ──────────────────────────────────────────
// 辅助函数
// ──────────────────────────────────────────

func getArgs(ctx *zero.Ctx) string {
	if args, ok := ctx.State["args"].(string); ok {
		return strings.TrimSpace(args)
	}
	return ""
}

func extractImages(ctx *zero.Ctx) []string {
	var images []string
	for _, seg := range ctx.Event.Message {
		if seg.Type == "image" {
			if u := seg.Data["url"]; u != "" {
				images = append(images, u)
			} else if f := seg.Data["file"]; f != "" {
				images = append(images, f)
			}
		}
	}
	return images
}

func parseIDs(s string) ([]int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("编号不能为空")
	}

	if strings.Contains(s, "-") {
		parts := strings.SplitN(s, "-", 2)
		start, err1 := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
		end, err2 := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("格式错误，应为 1-4")
		}
		if start > end {
			start, end = end, start
		}
		if end-start > 20 {
			return nil, fmt.Errorf("一次最多处理20条")
		}
		var ids []int64
		for i := start; i <= end; i++ {
			ids = append(ids, i)
		}
		return ids, nil
	}

	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '，'
	})
	var ids []int64
	for _, p := range parts {
		if p == "" {
			continue
		}
		id, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("编号 %s 错误", p)
		}
		ids = append(ids, id)
	}
	return ids, nil
}
