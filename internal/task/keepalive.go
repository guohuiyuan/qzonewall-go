package task

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"log"
	"strings"
	"time"

	qzone "github.com/guohuiyuan/qzone-go"
	"github.com/guohuiyuan/qzonewall-go/internal/config"
	"github.com/guohuiyuan/qzonewall-go/internal/rkey"
	zero "github.com/wdvxdr1123/ZeroBot"
	"github.com/wdvxdr1123/ZeroBot/message"
)

// KeepAlive 定期校验 QQ 空间 Cookie 有效性并自动刷新。
type KeepAlive struct {
	qzoneCfg config.QzoneConfig
	botCfg   config.BotConfig
	client   *qzone.Client
	ctx      context.Context
	cancel   context.CancelFunc
}

type CookieResult struct {
	Cookie string
	Err    error
}

func NewKeepAlive(qzoneCfg config.QzoneConfig, botCfg config.BotConfig, client *qzone.Client) *KeepAlive {
	ctx, cancel := context.WithCancel(context.Background())
	return &KeepAlive{qzoneCfg: qzoneCfg, botCfg: botCfg, client: client, ctx: ctx, cancel: cancel}
}

func (k *KeepAlive) Start() {
	if k.qzoneCfg.KeepAlive <= 0 {
		log.Println("[KeepAlive] disabled (keep_alive <= 0)")
		return
	}
	go k.run()
	log.Printf("[KeepAlive] started, interval=%v", k.qzoneCfg.KeepAlive)
}

func (k *KeepAlive) Stop() { k.cancel() }

func (k *KeepAlive) run() {
	ticker := time.NewTicker(k.qzoneCfg.KeepAlive)
	defer ticker.Stop()

	for {
		select {
		case <-k.ctx.Done():
			log.Println("[KeepAlive] stopped")
			return
		case <-ticker.C:
			k.check()
		}
	}
}

func (k *KeepAlive) check() {
	log.Println("[KeepAlive] validating cookie via GetUserInfo...")
	if _, err := validateCookieWithUserInfo(k.ctx, k.client); err == nil {
		log.Println("[KeepAlive] cookie valid")
		return
	}

	log.Println("[KeepAlive] cookie invalid, trying refresh from bot")
	if k.tryRefreshFromBot() {
		return
	}

	k.notifyAdmin("⚠️ QQ空间 Cookie 已过期，请使用 /扫码 或 /刷新cookie 重新登录")
}

func (k *KeepAlive) tryRefreshFromBot() bool {
	var refreshed bool
	zero.RangeBot(func(id int64, ctx *zero.Ctx) bool {
		_, _ = rkey.UpdateFromRaw(ctx.NcGetRKey().Raw)

		cookie := ctx.GetCookies("qzone.qq.com")
		if cookie == "" {
			return true
		}
		if err := k.client.UpdateCookie(cookie); err != nil {
			log.Printf("[KeepAlive] refresh from bot(%d) failed: %v", id, err)
			return true
		}
		log.Printf("[KeepAlive] refreshed from bot(%d), UIN=%d", id, k.client.UIN())
		refreshed = true
		return false
	})
	return refreshed
}

// EnsureCookieValidOnStartup validates cookie once during startup and
// attempts a single refresh flow when invalid.
func EnsureCookieValidOnStartup(_ config.QzoneConfig, botCfg config.BotConfig, client *qzone.Client) error {
	if client == nil {
		return fmt.Errorf("nil qzone client")
	}

	log.Println("[Startup] validating cookie via GetUserInfo...")
	info, err := validateCookieWithUserInfo(context.Background(), client)
	if err == nil {
		name := strings.TrimSpace(info.Nickname)
		if name == "" {
			name = "(empty)"
		}
		log.Printf("[Startup] cookie valid, uin=%d, nickname=%s", info.UIN, name)
		return nil
	}
	log.Printf("[Startup] cookie invalid: %v", err)

	// qzone.Client may already have refreshed via OnSessionExpired; re-check once.
	time.Sleep(500 * time.Millisecond)
	if info, recheckErr := validateCookieWithUserInfo(context.Background(), client); recheckErr == nil {
		log.Printf("[Startup] cookie became valid after internal refresh, uin=%d, nickname=%s", info.UIN, strings.TrimSpace(info.Nickname))
		return nil
	}

	refreshFn := RefreshCookie(botCfg)
	newCookie, refreshErr := refreshFn()
	if refreshErr != nil {
		return fmt.Errorf("startup refresh failed: %w", refreshErr)
	}
	if updateErr := client.UpdateCookie(newCookie); updateErr != nil {
		return fmt.Errorf("startup update cookie failed: %w", updateErr)
	}

	info, err = validateCookieWithUserInfo(context.Background(), client)
	if err != nil {
		return fmt.Errorf("cookie still invalid after refresh: %w", err)
	}
	log.Printf("[Startup] cookie refresh success, uin=%d, nickname=%s", info.UIN, strings.TrimSpace(info.Nickname))
	return nil
}

func validateCookieWithUserInfo(parent context.Context, client *qzone.Client) (*qzone.UserInfo, error) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	return client.GetMyInfo(ctx)
}

func (k *KeepAlive) notifyAdmin(text string) {
	if k.botCfg.ManageGroup <= 0 {
		return
	}
	zero.RangeBot(func(id int64, ctx *zero.Ctx) bool {
		ctx.SendGroupMessage(k.botCfg.ManageGroup, message.Text(text))
		return false
	})
}

// TryGetCookie sources cookie from two methods in fixed order:
// 1) ZeroBot GetCookies
// 2) QR login
func TryGetCookie(_ config.QzoneConfig) (string, error) {
	// 优化：启动后先硬等待 2 秒。
	// 原因：Bot 连接 WS 和同步 Cookie 需要几百毫秒到 1 秒的时间。
	// 直接循环会导致第一次必定失败，不如先等一下，通常能一次命中。
	log.Println("[Init] 启动预热：等待 2秒 获取 Bot 状态...")
	time.Sleep(2 * time.Second)

	const maxAttempts = 5
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// 尝试从 Bot 获取
		cookie, ok := tryGetCookieFromBots(fmt.Sprintf("[Init-%d]", attempt))
		if ok {
			log.Println("[Init] ✅ 成功从 Bot 获取到 Cookie")
			return cookie, nil
		}

		if attempt < maxAttempts {
			time.Sleep(1 * time.Second)
		}
	}

	log.Println("[Init] 所有 Bot 均未返回有效 Cookie，降级使用二维码登录")
	return tryQRLogin()
}

// TryGetCookieAsync runs the full cookie bootstrap flow in background:
// 1) ZeroBot GetCookies retries
// 2) fallback QR login
func TryGetCookieAsync(qzoneCfg config.QzoneConfig) <-chan CookieResult {
	ch := make(chan CookieResult, 1)
	go func() {
		defer close(ch)
		cookie, err := TryGetCookie(qzoneCfg)
		ch <- CookieResult{Cookie: cookie, Err: err}
	}()
	return ch
}

// RefreshCookie is used by qzone.WithOnSessionExpired callback.
func RefreshCookie(botCfg config.BotConfig) func() (string, error) {
	return func() (string, error) {
		log.Println("[SessionExpired] cookie expired, trying bot GetCookies...")
		cookie, ok := tryGetCookieFromBots("[SessionExpired]")
		if ok {
			return cookie, nil
		}

		if botCfg.ManageGroup > 0 {
			zero.RangeBot(func(id int64, ctx *zero.Ctx) bool {
				ctx.SendGroupMessage(botCfg.ManageGroup, message.Text("⚠️ QQ空间 Cookie 过期，GetCookies 刷新失败，请使用 /扫码 重新登录"))
				return false
			})
		}
		return "", fmt.Errorf("cookie refresh failed; please scan QR manually")
	}
}

func tryGetCookieFromBots(prefix string) (string, bool) {
	seenBots := 0
	var cookie string
	zero.RangeBot(func(id int64, ctx *zero.Ctx) bool {
		seenBots++
		c := ctx.GetCookies("qzone.qq.com")
		if c == "" {
			log.Printf("%s bot(%d) GetCookies empty", prefix, id)
			return true
		}
		log.Printf("%s bot(%d) GetCookies=%s", prefix, id, c)
		cookie = c
		return false
	})
	if seenBots == 0 {
		log.Printf("%s no bot context available for GetCookies", prefix)
	}
	return cookie, cookie != ""
}

func tryQRLogin() (string, error) {
	log.Println("[Init] trying QR login...")

	qr, err := qzone.GetQRCode()
	if err != nil {
		return "", fmt.Errorf("get qrcode failed: %w", err)
	}
	printQRCodeInTerminal(qr.Image)

	for i := 0; i < 120; i++ {
		time.Sleep(2 * time.Second)
		state, cookie, err := qzone.PollQRLogin(qr)
		if err != nil {
			return "", fmt.Errorf("poll qrcode failed: %w", err)
		}
		switch state {
		case qzone.LoginSuccess:
			log.Println("[Init] QR login success")
			return cookie, nil
		case qzone.LoginExpired:
			return "", fmt.Errorf("qrcode expired")
		case qzone.LoginScanned:
			log.Println("[Init] QR scanned, waiting confirm...")
		}
	}
	return "", fmt.Errorf("qrcode login timeout")
}

func printQRCodeInTerminal(pngData []byte) {
	img, _, err := image.Decode(bytes.NewReader(pngData))
	if err != nil {
		log.Printf("[Init] print qr in terminal failed: %v", err)
		return
	}

	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w == 0 || h == 0 {
		return
	}

	// Add a white border for easier scan.
	log.Println("[Init] scan QR in terminal:")
	border := strings.Repeat("  ", w+4)
	log.Println(border)
	for y := 0; y < h; y++ {
		var sb strings.Builder
		sb.WriteString("    ")
		for x := 0; x < w; x++ {
			r, g, b, _ := img.At(x+b.Min.X, y+b.Min.Y).RGBA()
			gray := (r + g + b) / 3
			if gray < 0x8000 {
				sb.WriteString("██")
			} else {
				sb.WriteString("  ")
			}
		}
		sb.WriteString("    ")
		log.Println(sb.String())
	}
	log.Println(border)
}
