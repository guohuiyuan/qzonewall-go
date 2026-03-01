package main

import (
	_ "embed"
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
	"github.com/spf13/cobra"
)

//go:embed example_config.json
var exampleConfig string

var (
	cfgPath string
	port    string
)

func main() {
	var rootCmd = &cobra.Command{
		Use:   "wall",
		Short: "Qzone Wall Bot",
		Run:   runApp,
	}

	rootCmd.PersistentFlags().StringVarP(&cfgPath, "config", "c", "data/config.json", "config file path")
	rootCmd.PersistentFlags().StringVarP(&port, "port", "p", "8080", "web server port")

	if err := rootCmd.Execute(); err != nil {
		log.Println(err)
		os.Exit(1)
	}
}

func runApp(cmd *cobra.Command, args []string) {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// 确保 data 目录存在
	if err := os.MkdirAll("data/uploads", 0755); err != nil {
		log.Fatalf("create data directory failed: %v", err)
	}

	// 检查配置文件是否存在，如果不存在则生成示例配置
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		if err := os.WriteFile(cfgPath, []byte(exampleConfig), 0644); err != nil {
			log.Fatalf("create example config failed: %v", err)
		}
		log.Printf("[Main] 已生成示例配置文件 %s", cfgPath)
		log.Println("[Main] 默认管理员账号: admin / admin123，可在管理后台「系统设置」中修改配置和密码")
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("load config failed: %v", err)
	}

	// Override web addr using cobra port
	if port != "" {
		host := ""
		if idx := strings.LastIndex(cfg.Web.Addr, ":"); idx != -1 {
			host = cfg.Web.Addr[:idx]
		}
		cfg.Web.Addr = host + ":" + port
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

	qqBot := source.NewQQBot(cfg, st, renderer, nil, censorWords)
	if err := qqBot.Start(); err != nil {
		log.Fatalf("start qq bot failed: %v", err)
	}
	log.Println("[Main] qq bot started")

	// Start with a valid-form placeholder cookie to avoid blocking startup.
	// Real cookie bootstrap (GetCookies -> QR fallback) runs asynchronously below.
	initCookie := "uin=o1;skey=@bootstrap;p_skey=bootstrap"

	qzClient, err := qzone.NewClient(initCookie,
		qzone.WithTimeout(cfg.Qzone.Timeout.Duration),
		qzone.WithMaxRetry(cfg.Qzone.MaxRetry),
		qzone.WithOnSessionExpired(task.RefreshCookie(cfg)),
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
		res := <-task.TryGetCookieAsync(cfg)
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

		if err := task.EnsureCookieValidOnStartup(cfg, qzClient); err != nil {
			log.Printf("[Main] startup cookie validation failed: %v", err)
		}
	}()

	qqBot.SetClient(qzClient)

	worker := task.NewWorker(cfg, qzClient, st, renderer)
	worker.Start()
	defer worker.Stop()

	keepAlive := task.NewKeepAlive(cfg, qzClient)
	keepAlive.Start()
	defer keepAlive.Stop()

	if cfg.Web.Enable {
		webServer := web.NewServer(cfg, cfgPath, st, qzClient, renderer)
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
