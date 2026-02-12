package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	qzone "github.com/guohuiyuan/qzone-go"
	"github.com/guohuiyuan/qzonewall-go/internal/config"
	"github.com/guohuiyuan/qzonewall-go/internal/model"
	"github.com/guohuiyuan/qzonewall-go/internal/render"
	"github.com/guohuiyuan/qzonewall-go/internal/rkey"
	"github.com/guohuiyuan/qzonewall-go/internal/store"
	zero "github.com/wdvxdr1123/ZeroBot"
)

//go:embed templates/*.html templates/icon.png
var templateFS embed.FS

// Server Web 服务。
type Server struct {
	cfg       config.WebConfig
	wallCfg   config.WallConfig
	store     *store.Store
	qzClient  *qzone.Client
	renderer  *render.Renderer
	tmpl      *template.Template
	server    *http.Server
	uploadDir string

	// QR 登录状态
	qrMu      sync.Mutex
	qrCode    *qzone.QRCode
	qrStatus  string // "", "waiting", "scanned", "success", "expired", "error"
	qrMessage string
}

// NewServer 创建 Web 服务实例。
func NewServer(
	cfg config.WebConfig,
	wallCfg config.WallConfig,
	st *store.Store,
	qzClient *qzone.Client,
	renderer *render.Renderer,
) *Server {
	return &Server{
		cfg:       cfg,
		wallCfg:   wallCfg,
		store:     st,
		qzClient:  qzClient,
		renderer:  renderer,
		uploadDir: "uploads",
	}
}

// Start 启动 HTTP 服务。
func (s *Server) Start() error {
	funcMap := template.FuncMap{
		"formatTime": func(ts int64) string {
			return time.Unix(ts, 0).Format("2006-01-02 15:04")
		},
		"statusText": func(st model.PostStatus) string {
			m := map[model.PostStatus]string{
				model.StatusPending:   "待审核",
				model.StatusApproved:  "已通过",
				model.StatusRejected:  "已拒绝",
				model.StatusFailed:    "失败",
				model.StatusPublished: "已发布",
			}
			if v, ok := m[st]; ok {
				return v
			}
			return string(st)
		},
		"statusClass": func(st model.PostStatus) string {
			m := map[model.PostStatus]string{
				model.StatusPending:   "pending",
				model.StatusApproved:  "approved",
				model.StatusRejected:  "rejected",
				model.StatusFailed:    "failed",
				model.StatusPublished: "published",
			}
			return m[st]
		},
		"hasImages": func(imgs []string) bool { return len(imgs) > 0 },
	}

	var err error
	s.tmpl, err = template.New("").Funcs(funcMap).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return fmt.Errorf("parse templates: %w", err)
	}

	if err := os.MkdirAll(s.uploadDir, 0755); err != nil {
		return fmt.Errorf("create upload dir: %w", err)
	}

	if err := s.initAdmin(); err != nil {
		log.Printf("[Web] 初始化管理员账号失败: %v", err)
	}

	mux := http.NewServeMux()

	// 页面路由
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.handleLogout)
	mux.HandleFunc("/submit", s.handleSubmitPage)
	mux.HandleFunc("/admin", s.handleAdminPage)
	mux.HandleFunc("/icon.png", s.handleIcon)
	mux.HandleFunc("/favicon.ico", s.handleFavicon)

	// API 路由
	mux.HandleFunc("/api/submit", s.handleAPISubmit)
	mux.HandleFunc("/api/approve", s.handleAPIApprove)
	mux.HandleFunc("/api/reject", s.handleAPIReject)
	mux.HandleFunc("/api/approve/batch", s.handleAPIBatchApprove)
	mux.HandleFunc("/api/reject/batch", s.handleAPIBatchReject)
	mux.HandleFunc("/api/qrcode", s.handleAPIQRCode)
	mux.HandleFunc("/api/qrcode/status", s.handleAPIQRStatus)
	mux.HandleFunc("/api/health", s.handleAPIHealth)
	mux.HandleFunc("/api/qzone/status", s.handleAPIQzoneStatus)
	mux.HandleFunc("/api/qzone/refresh", s.handleAPIQzoneRefresh)

	// 静态资源
	mux.Handle("/uploads/", http.StripPrefix("/uploads/", http.FileServer(http.Dir(s.uploadDir))))

	s.server = &http.Server{
		Addr:    s.cfg.Addr,
		Handler: mux,
	}

	go func() {
		urlStr := localWebURL(s.cfg.Addr)
		log.Printf("[Web] 监听 %s (%s)", s.cfg.Addr, urlStr)
		go func() {
			time.Sleep(500 * time.Millisecond)
			openBrowser(urlStr)
		}()
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[Web] 服务异常: %v", err)
		}
	}()
	return nil
}

// Stop 停止服务。
func (s *Server) Stop() {
	if s.server != nil {
		_ = s.server.Close()
		log.Println("[Web] stopped")
	}
}

func (s *Server) initAdmin() error {
	count, err := s.store.AccountCount()
	if err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	salt := randomHex(16)
	hash := hashPassword(s.cfg.AdminPass, salt)
	return s.store.CreateAccount(s.cfg.AdminUser, hash, salt, "admin")
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	account := s.currentAccount(r)
	if account != nil && account.IsAdmin() {
		http.Redirect(w, r, "/admin", http.StatusFound)
	} else {
		http.Redirect(w, r, "/submit", http.StatusFound)
	}
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		account := s.currentAccount(r)
		if account != nil && account.IsAdmin() {
			http.Redirect(w, r, "/admin", http.StatusFound)
			return
		}
		s.renderTemplate(w, "login.html", nil)
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")

	account, err := s.store.GetAccount(username)
	if err != nil || account == nil {
		s.renderTemplate(w, "login.html", map[string]string{"Error": "用户名或密码错误"})
		return
	}
	if hashPassword(password, account.Salt) != account.PasswordHash {
		s.renderTemplate(w, "login.html", map[string]string{"Error": "用户名或密码错误"})
		return
	}
	if !account.IsAdmin() {
		s.renderTemplate(w, "login.html", map[string]string{"Error": "仅管理员可登录"})
		return
	}

	token := randomHex(32)
	expire := time.Now().Add(24 * time.Hour).Unix()
	if err := s.store.CreateSession(token, account.ID, expire); err != nil {
		s.renderTemplate(w, "login.html", map[string]string{"Error": "登录失败"})
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    token,
		Path:     "/",
		MaxAge:   86400,
		HttpOnly: true,
	})

	http.Redirect(w, r, "/admin", http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("session"); err == nil {
		_ = s.store.DeleteSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:   "session",
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
	http.Redirect(w, r, "/submit", http.StatusFound)
}

func (s *Server) handleSubmitPage(w http.ResponseWriter, r *http.Request) {
	account := s.currentAccount(r)
	data := map[string]interface{}{
		"Account":   account,
		"IsAdmin":   account != nil && account.IsAdmin(),
		"MaxImages": s.wallCfg.MaxImages,
		"Message":   r.URL.Query().Get("msg"),
	}
	s.renderTemplate(w, "user.html", data)
}

func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	account := s.currentAccount(r)
	if account == nil || !account.IsAdmin() {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	statusFilter := r.URL.Query().Get("status")
	var posts []*model.Post
	var err error
	if statusFilter != "" {
		posts, err = s.store.ListByStatus(model.PostStatus(statusFilter))
	} else {
		posts, err = s.store.ListAll(100, 0)
	}
	if err != nil {
		log.Printf("[Web] 查询投稿失败: %v", err)
	}

	s.refreshInvalidRKeyInPosts(posts)

	// 🟢 修改：构建用于展示的 Post 列表，将 file ID 还原为 URL
	displayPosts := make([]*model.Post, len(posts))
	for i, p := range posts {
		// 克隆并解析图片链接
		displayPosts[i] = s.resolvePostImages(p)
	}

	totalCount, _ := s.store.CountAll()
	pendingCount, _ := s.store.CountByStatus(model.StatusPending)
	approvedCount, _ := s.store.CountByStatus(model.StatusApproved)
	rejectedCount, _ := s.store.CountByStatus(model.StatusRejected)
	publishedCount, _ := s.store.CountByStatus(model.StatusPublished)

	data := map[string]interface{}{
		"Account":        account,
		"Posts":          displayPosts, // 使用解析后的列表
		"TotalCount":     totalCount,
		"PendingCount":   pendingCount,
		"ApprovedCount":  approvedCount,
		"RejectedCount":  rejectedCount,
		"PublishedCount": publishedCount,
		"StatusFilter":   statusFilter,
		"CookieValid":    s.isQzoneLoggedIn(),
		"QzoneUIN":       int64(0),
		"Message":        r.URL.Query().Get("msg"),
	}
	if s.qzClient != nil {
		data["QzoneUIN"] = s.qzClient.UIN()
	}

	s.renderTemplate(w, "admin.html", data)
}

func (s *Server) handleAPISubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonResp(w, 405, false, "仅支持 POST")
		return
	}

	account := s.currentAccount(r)

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		jsonResp(w, 400, false, "请求体过大")
		return
	}

	text := r.FormValue("text")
	name := r.FormValue("uin")
	uin, _ := strconv.ParseInt(name, 10, 64)
	anon := r.FormValue("anon") == "on" || r.FormValue("anon") == "true"
	if name == "" && account != nil {
		name = account.Username
	}
	if name == "" {
		name = "匿名用户"
	}

	var images []string
	files := r.MultipartForm.File["images"]
	for _, fh := range files {
		if len(images) >= s.wallCfg.MaxImages {
			break
		}
		f, err := fh.Open()
		if err != nil {
			continue
		}

		ext := filepath.Ext(fh.Filename)
		if ext == "" {
			ext = ".jpg"
		}
		filename := fmt.Sprintf("%d_%s%s", time.Now().UnixNano(), randomHex(8), ext)
		dst, err := os.Create(filepath.Join(s.uploadDir, filename))
		if err != nil {
			_ = f.Close()
			continue
		}
		_, _ = io.Copy(dst, f)
		_ = f.Close()
		_ = dst.Close()

		images = append(images, "/uploads/"+filename)
	}

	if text == "" && len(images) == 0 {
		jsonResp(w, 400, false, "内容不能为空")
		return
	}

	post := &model.Post{
		UIN:        uin,
		Name:       name,
		Text:       text,
		Images:     images,
		Anon:       anon,
		Status:     model.StatusPending,
		CreateTime: time.Now().Unix(),
	}
	if err := s.store.SavePost(post); err != nil {
		jsonResp(w, 500, false, "保存失败")
		return
	}

	log.Printf("[Web] received post #%d from %s", post.ID, name)
	jsonRespData(w, 200, true, fmt.Sprintf("投稿成功，编号 #%d，等待审核", post.ID), post.ID)
}

func (s *Server) handleAPIApprove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonResp(w, 405, false, "仅支持 POST")
		return
	}
	account := s.currentAccount(r)
	if account == nil || !account.IsAdmin() {
		jsonResp(w, 403, false, "无权限")
		return
	}

	idStr := r.FormValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		jsonResp(w, 400, false, "编号格式错误")
		return
	}
	post, err := s.store.GetPost(id)
	if err != nil || post == nil {
		jsonResp(w, 404, false, "稿件不存在")
		return
	}

	post.Status = model.StatusApproved
	if err := s.store.SavePost(post); err != nil {
		jsonResp(w, 500, false, "更新失败")
		return
	}
	jsonResp(w, 200, true, fmt.Sprintf("稿件 #%d 已通过", id))
}

func (s *Server) handleAPIReject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonResp(w, 405, false, "仅支持 POST")
		return
	}
	account := s.currentAccount(r)
	if account == nil || !account.IsAdmin() {
		jsonResp(w, 403, false, "无权限")
		return
	}

	idStr := r.FormValue("id")
	reason := r.FormValue("reason")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		jsonResp(w, 400, false, "编号格式错误")
		return
	}
	post, err := s.store.GetPost(id)
	if err != nil || post == nil {
		jsonResp(w, 404, false, "稿件不存在")
		return
	}

	post.Status = model.StatusRejected
	post.Reason = reason
	if err := s.store.SavePost(post); err != nil {
		jsonResp(w, 500, false, "更新失败")
		return
	}
	jsonResp(w, 200, true, fmt.Sprintf("稿件 #%d 已拒绝", id))
}

func (s *Server) handleAPIBatchApprove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonResp(w, 405, false, "仅支持 POST")
		return
	}
	account := s.currentAccount(r)
	if account == nil || !account.IsAdmin() {
		jsonResp(w, 403, false, "无权限")
		return
	}

	ids, err := parseBatchIDs(r.FormValue("ids"))
	if err != nil {
		jsonResp(w, 400, false, err.Error())
		return
	}

	posts, err := s.store.GetPostsByIDs(ids)
	if err != nil {
		jsonResp(w, 500, false, "数据库查询失败: "+err.Error())
		return
	}

	var validPosts []*model.Post
	for _, p := range posts {
		if p.Status == model.StatusPending {
			validPosts = append(validPosts, p)
		}
	}

	if len(validPosts) == 0 {
		jsonResp(w, 400, false, "没有待审核的稿件，或已处理")
		return
	}

	var summaryBuilder strings.Builder
	summaryBuilder.WriteString(fmt.Sprintf("【表白墙更新】 %s\n", time.Now().Format("01/02")))
	summaryBuilder.WriteString("----------------\n")

	var imagesData [][]byte

	for _, post := range validPosts {
		var imgData []byte
		var renderErr error

		if s.renderer != nil && s.renderer.Available() {
			// 解析 fileID 为 URL
			renderPost := s.resolvePostImages(post)
			imgData, renderErr = s.renderer.RenderPost(renderPost)
		} else {
			renderErr = fmt.Errorf("renderer not available")
		}

		if renderErr != nil || len(imgData) == 0 {
			log.Printf("[Web] 渲染失败 #%d: %v", post.ID, renderErr)
			continue
		}
		imagesData = append(imagesData, imgData)

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

		post.Status = model.StatusPublished
		_ = s.store.SavePost(post)
	}

	if len(imagesData) == 0 {
		jsonResp(w, 500, false, "没有成功渲染的图片，取消发布")
		return
	}

	summaryBuilder.WriteString("----------------\n")
	summaryBuilder.WriteString("详情见图 👇")
	finalText := summaryBuilder.String()

	opts := &qzone.PublishOption{
		ImageBytes: imagesData,
	}

	_, publishErr := s.qzClient.Publish(context.Background(), finalText, opts)

	if publishErr != nil {
		log.Printf("[Web] 发布说说失败: %v", publishErr)
		for _, p := range validPosts {
			p.Status = model.StatusPending
			_ = s.store.SavePost(p)
		}
		jsonResp(w, 500, false, "发布到QQ空间失败: "+publishErr.Error())
		return
	}

	jsonResp(w, 200, true, fmt.Sprintf("成功发布 %d 条稿件！", len(imagesData)))
}

func (s *Server) handleAPIBatchReject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonResp(w, 405, false, "仅支持 POST")
		return
	}
	account := s.currentAccount(r)
	if account == nil || !account.IsAdmin() {
		jsonResp(w, 403, false, "无权限")
		return
	}

	ids, err := parseBatchIDs(r.FormValue("ids"))
	if err != nil {
		jsonResp(w, 400, false, err.Error())
		return
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	updated, skipped, err := s.applyBatchStatus(ids, model.StatusRejected, reason)
	if err != nil {
		jsonResp(w, 500, false, "批量拒绝失败")
		return
	}
	jsonResp(w, 200, true, fmt.Sprintf("批量拒绝完成：成功 %d，跳过 %d", updated, skipped))
}

func parseBatchIDs(raw string) ([]int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("请先选择稿件")
	}

	parts := strings.Split(raw, ",")
	ids := make([]int64, 0, len(parts))
	seen := make(map[int64]struct{}, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		id, err := strconv.ParseInt(p, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("编号格式错误")
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("请先选择稿件")
	}
	return ids, nil
}

func (s *Server) applyBatchStatus(ids []int64, status model.PostStatus, reason string) (updated int, skipped int, err error) {
	posts, err := s.store.GetPostsByIDs(ids)
	if err != nil {
		return 0, 0, err
	}
	if len(posts) == 0 {
		return 0, len(ids), nil
	}

	for _, post := range posts {
		if post == nil {
			skipped++
			continue
		}
		if post.Status != model.StatusPending {
			skipped++
			continue
		}
		post.Status = status
		if status == model.StatusRejected {
			post.Reason = reason
		} else {
			post.Reason = ""
		}
		if err := s.store.SavePost(post); err != nil {
			return updated, skipped, err
		}
		updated++
	}
	missing := len(ids) - len(posts)
	if missing > 0 {
		skipped += missing
	}
	return updated, skipped, nil
}

func (s *Server) handleAPIQRCode(w http.ResponseWriter, r *http.Request) {
	account := s.currentAccount(r)
	if account == nil || !account.IsAdmin() {
		jsonResp(w, 403, false, "无权限")
		return
	}

	qr, err := qzone.GetQRCode()
	if err != nil {
		jsonResp(w, 500, false, "获取二维码失败: "+err.Error())
		return
	}

	s.qrMu.Lock()
	s.qrCode = qr
	s.qrStatus = "waiting"
	s.qrMessage = ""
	s.qrMu.Unlock()

	go s.pollQRLogin()

	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(qr.Image)
}

func (s *Server) pollQRLogin() {
	s.qrMu.Lock()
	qr := s.qrCode
	s.qrMu.Unlock()
	if qr == nil {
		return
	}

	for i := 0; i < 120; i++ {
		time.Sleep(2 * time.Second)
		state, cookie, err := qzone.PollQRLogin(qr)
		if err != nil {
			s.qrMu.Lock()
			s.qrStatus = "error"
			s.qrMessage = err.Error()
			s.qrMu.Unlock()
			return
		}
		switch state {
		case qzone.LoginSuccess:
			if err := s.qzClient.UpdateCookie(cookie); err != nil {
				s.qrMu.Lock()
				s.qrStatus = "error"
				s.qrMessage = "Cookie 更新失败: " + err.Error()
				s.qrMu.Unlock()
				return
			}
			s.qrMu.Lock()
			s.qrStatus = "success"
			s.qrMessage = fmt.Sprintf("登录成功, UIN=%d", s.qzClient.UIN())
			s.qrMu.Unlock()
			return
		case qzone.LoginExpired:
			s.qrMu.Lock()
			s.qrStatus = "expired"
			s.qrMessage = "二维码已过期"
			s.qrMu.Unlock()
			return
		case qzone.LoginScanned:
			s.qrMu.Lock()
			s.qrStatus = "scanned"
			s.qrMu.Unlock()
		}
	}

	s.qrMu.Lock()
	s.qrStatus = "expired"
	s.qrMessage = "登录超时"
	s.qrMu.Unlock()
}

func (s *Server) handleAPIQRStatus(w http.ResponseWriter, r *http.Request) {
	s.qrMu.Lock()
	status := s.qrStatus
	msg := s.qrMessage
	s.qrMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  status,
		"message": msg,
	})
}

func (s *Server) handleAPIHealth(w http.ResponseWriter, r *http.Request) {
	jsonResp(w, 200, true, "ok")
}

func (s *Server) handleAPIQzoneStatus(w http.ResponseWriter, r *http.Request) {
	account := s.currentAccount(r)
	if account == nil || !account.IsAdmin() {
		jsonResp(w, http.StatusForbidden, false, "forbidden")
		return
	}

	uin := int64(0)
	if s.qzClient != nil {
		uin = s.qzClient.UIN()
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":           true,
		"cookie_valid": s.isQzoneLoggedIn(),
		"uin":          uin,
	})
}

func (s *Server) handleAPIQzoneRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonResp(w, 405, false, "仅支持 POST")
		return
	}
	account := s.currentAccount(r)
	if account == nil || !account.IsAdmin() {
		jsonResp(w, 403, false, "无权限")
		return
	}

	var success bool
	var uin int64

	zero.RangeBot(func(id int64, ctx *zero.Ctx) bool {
		if rk := ctx.NcGetRKey(); rk.Raw != "" {
			_, _ = rkey.UpdateFromRaw(rk.Raw)
		}

		cookie := ctx.GetCookies("qzone.qq.com")
		if cookie == "" {
			return true
		}

		if err := s.qzClient.UpdateCookie(cookie); err != nil {
			log.Printf("[Web] 从 Bot(%d) 刷新 Cookie 失败: %v", id, err)
			return true
		}

		uin = s.qzClient.UIN()
		success = true
		log.Printf("[Web] 成功从 Bot(%d) 拉取 Cookie, UIN=%d", id, uin)
		return false
	})

	if success {
		jsonResp(w, 200, true, fmt.Sprintf("成功从 Bot 拉取 Cookie (UIN: %d)", uin))
	} else {
		jsonResp(w, 200, false, "未能从任何 Bot 获取到有效 Cookie")
	}
}

func (s *Server) handleIcon(w http.ResponseWriter, r *http.Request) {
	icon, err := templateFS.ReadFile("templates/icon.png")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(icon)
}

func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	s.handleIcon(w, r)
}

func (s *Server) currentAccount(r *http.Request) *model.Account {
	c, err := r.Cookie("session")
	if err != nil {
		return nil
	}
	accountID, err := s.store.GetSession(c.Value)
	if err != nil || accountID == 0 {
		return nil
	}
	account, err := s.store.GetAccountByID(accountID)
	if err != nil {
		return nil
	}
	return account
}

func hashPassword(password, salt string) string {
	h := sha256.New()
	h.Write([]byte(salt + password))
	return hex.EncodeToString(h.Sum(nil))
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func jsonResp(w http.ResponseWriter, status int, ok bool, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":      ok,
		"message": msg,
	})
}

func jsonRespData(w http.ResponseWriter, status int, ok bool, msg string, postID int64) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":      ok,
		"message": msg,
		"post_id": postID,
	})
}

func (s *Server) renderTemplate(w http.ResponseWriter, name string, data interface{}) {
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("[Web] render template failed: %s: %v", name, err)
		http.Error(w, "template render error", http.StatusInternalServerError)
	}
}

func (s *Server) RegisterUser(username, password string) error {
	existing, _ := s.store.GetAccount(username)
	if existing != nil {
		return fmt.Errorf("用户名已存在")
	}
	salt := randomHex(16)
	hash := hashPassword(password, salt)
	return s.store.CreateAccount(username, hash, salt, "user")
}

func (s *Server) SetCookieFile(cookieFile string) {
	_ = cookieFile
}

func (s *Server) GetUploadDir() string {
	return s.uploadDir
}

func (s *Server) refreshInvalidRKeyInPosts(posts []*model.Post) {
	if len(posts) == 0 {
		return
	}
	rk := strings.TrimSpace(rkey.GetByType(10))
	if rk == "" {
		_ = rkey.RefreshFromBots()
		rk = strings.TrimSpace(rkey.GetByType(10))
	}
	if rk == "" {
		log.Printf("[Web] refresh rkey skipped: type=10 rkey not available")
		return
	}

	for _, post := range posts {
		if post == nil || len(post.Images) == 0 {
			continue
		}
		updated := false
		for i, raw := range post.Images {
			raw = strings.TrimSpace(raw)
			if !hasRKey(raw) {
				continue
			}
			fixed, err := replaceRKey(raw, rk)
			if err != nil {
				log.Printf("[Web] refresh rkey failed: %v | url=%s", err, raw)
				continue
			}

			post.Images[i] = fixed
			updated = true
			log.Printf("[Web] rkey refreshed: %s", fixed)
		}

		if updated {
			if err := s.store.SavePost(post); err != nil {
				log.Printf("[Web] save refreshed image urls failed: %v", err)
			}
		}
	}
}

func hasRKey(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.Query().Get("rkey") != ""
}

func replaceRKey(raw, rk string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("rkey", rk)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func localWebURL(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil && port != "" {
		return "http://localhost:" + port
	}
	if strings.HasPrefix(addr, ":") {
		port := strings.TrimPrefix(addr, ":")
		if port != "" {
			return "http://localhost:" + port
		}
	}
	if _, err := strconv.Atoi(addr); err == nil {
		return "http://localhost:" + addr
	}
	return "http://localhost:8080"
}

func openBrowser(url string) {
	var cmd string
	var args []string

	switch runtime.GOOS {
	case "windows":
		cmd, args = "cmd", []string{"/c", "start", ""}
	case "darwin":
		cmd = "open"
	default:
		cmd = "xdg-open"
	}
	args = append(args, url)
	_ = exec.Command(cmd, args...).Start()
}

func (s *Server) isQzoneLoggedIn() bool {
	if s.qzClient == nil || s.qzClient.UIN() <= 0 {
		return false
	}
	raw := s.qzClient.Session().Cookie()
	return !strings.Contains(raw, "p_skey=bootstrap")
}

// ── Image Resolution Helpers ──

func (s *Server) resolvePostImages(p *model.Post) *model.Post {
	clone := *p
	clone.Images = make([]string, len(p.Images))
	for i, img := range p.Images {
		clone.Images[i] = s.resolveImageURL(img)
	}
	return &clone
}

func (s *Server) resolveImageURL(img string) string {
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
