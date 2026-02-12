package main

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	qzone "github.com/guohuiyuan/qzone-go"
	"github.com/guohuiyuan/qzonewall-go/internal/config"
	"github.com/guohuiyuan/qzonewall-go/internal/render"
	"github.com/guohuiyuan/qzonewall-go/internal/source"
	"github.com/guohuiyuan/qzonewall-go/internal/store"
	"github.com/guohuiyuan/qzonewall-go/internal/task"
	"github.com/guohuiyuan/qzonewall-go/internal/web"
)

//go:embed example_config.yaml
var exampleConfig string

// generateRandomPassword 生成一个16字符的随机密码
func generateRandomPassword() string {
	bytes := make([]byte, 8) // 8字节 = 16个十六进制字符
	if _, err := rand.Read(bytes); err != nil {
		// 如果随机生成失败，使用默认密码
		return "admin123"
	}
	return hex.EncodeToString(bytes)
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	cfgPath := "config.yaml"
	for i := 1; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--config", "-c":
			if i+1 < len(os.Args) {
				i++
				cfgPath = os.Args[i]
			}
		default:
			cfgPath = os.Args[i]
		}
	}

	// 检查配置文件是否存在，如果不存在则生成示例配置
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		randomPass := generateRandomPassword()
		configContent := strings.Replace(exampleConfig, `admin_pass: ""`, `admin_pass: "`+randomPass+`"`, 1)
		if err := os.WriteFile(cfgPath, []byte(configContent), 0644); err != nil {
			log.Fatalf("create example config failed: %v", err)
		}
		log.Printf("[Main] example config.yaml generated with random password: %s, please edit it and restart", randomPass)
		os.Exit(0)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("load config failed: %v", err)
	}
	log.Println("[Main] config loaded")

	st, err := store.New(cfg.Database.Path)
	if err != nil {
		log.Fatalf("init sqlite failed: %v", err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Printf("[Main] close sqlite failed: %v", err)
		}
	}()
	log.Println("[Main] sqlite ready")

	censorWords := store.LoadCensorWords(cfg.Censor.Words, cfg.Censor.WordsFile)
	log.Printf("[Main] loaded censor words: %d", len(censorWords))

	renderer := render.NewRenderer()
	if renderer.Available() {
		log.Println("[Main] renderer enabled")
	} else {
		log.Println("[Main] renderer disabled")
	}

	qqBot := source.NewQQBot(cfg.Bot, cfg.Wall, cfg.Qzone, st, renderer, nil, censorWords)
	if err := qqBot.Start(); err != nil {
		log.Fatalf("start qq bot failed: %v", err)
	}
	log.Println("[Main] qq bot started")

	// Start with a valid-form placeholder cookie to avoid blocking startup.
	// Real cookie bootstrap (GetCookies -> QR fallback) runs asynchronously below.
	initCookie := "uin=o1;skey=@bootstrap;p_skey=bootstrap"

	qzClient, err := qzone.NewClient(initCookie,
		qzone.WithTimeout(cfg.Qzone.Timeout),
		qzone.WithMaxRetry(cfg.Qzone.MaxRetry),
		qzone.WithOnSessionExpired(task.RefreshCookie(cfg.Bot)),
	)
	if err != nil {
		log.Fatalf("[Main] qzone client create failed: %v", err)
	}

	if qzClient == nil {
		log.Fatal("[Main] qzone client is nil after initialization")
	}
	log.Println("[Main] qzone client created")

	go func() {
		log.Println("[Main] async cookie bootstrap started")
		res := <-task.TryGetCookieAsync(cfg.Qzone)
		if res.Err != nil {
			log.Printf("[Main] async cookie bootstrap failed: %v", res.Err)
			log.Println("[Main] use /扫码 or web admin QR login to refresh cookie")
			return
		}
		if err := qzClient.UpdateCookie(res.Cookie); err != nil {
			log.Printf("[Main] async cookie update failed: %v", err)
			return
		}
		log.Printf("[Main] async cookie bootstrap success, uin=%d", qzClient.UIN())

		if err := task.EnsureCookieValidOnStartup(cfg.Qzone, cfg.Bot, qzClient); err != nil {
			log.Printf("[Main] startup cookie validation failed: %v", err)
		}
	}()

	qqBot.SetClient(qzClient)

	worker := task.NewWorker(cfg.Worker, cfg.Wall, qzClient, st, renderer)
	worker.Start()
	defer worker.Stop()

	keepAlive := task.NewKeepAlive(cfg.Qzone, cfg.Bot, qzClient)
	keepAlive.Start()
	defer keepAlive.Stop()

	if cfg.Web.Enable {
		webServer := web.NewServer(cfg.Web, cfg.Wall, st, qzClient)
		go func() {
			if err := webServer.Start(); err != nil {
				log.Printf("[Main] web server stopped: %v", err)
			}
		}()
		defer webServer.Stop()
		log.Printf("[Main] web server started: %s", cfg.Web.Addr)
	}

	log.Println("[Main] system started")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	s := <-sig
	log.Printf("[Main] got signal %v, shutting down...", s)
}
