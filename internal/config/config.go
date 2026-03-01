package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Duration 是 time.Duration 的包装类型，支持 JSON 字符串序列化（如 "10s"）
type Duration struct {
	time.Duration
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var v interface{}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch val := v.(type) {
	case string:
		dur, err := time.ParseDuration(val)
		if err != nil {
			return err
		}
		d.Duration = dur
	case float64:
		d.Duration = time.Duration(int64(val))
	default:
		return fmt.Errorf("invalid duration type: %T", v)
	}
	return nil
}

// Config 应用总配置
type Config struct {
	Qzone    QzoneConfig    `json:"qzone"`
	Bot      BotConfig      `json:"bot"`
	Wall     WallConfig     `json:"wall"`
	Database DatabaseConfig `json:"database"`
	Web      WebConfig      `json:"web"`
	Censor   CensorConfig   `json:"censor"`
	Worker   WorkerConfig   `json:"worker"`
	Log      LogConfig      `json:"log"`
}

// QzoneConfig QQ空间账号配置
type QzoneConfig struct {
	KeepAlive Duration `json:"keep_alive"`
	MaxRetry  int      `json:"max_retry"`
	Timeout   Duration `json:"timeout"`
}

// BotConfig QQ机器人配置
type BotConfig struct {
	Zero        ZeroBotConfig `json:"zero"`
	WS          []WSConfig    `json:"ws"`
	ManageGroup int64         `json:"manage_group"`
}

// ZeroBotConfig ZeroBot 核心配置
type ZeroBotConfig struct {
	NickName       []string `json:"nickname"`
	CommandPrefix  string   `json:"command_prefix"`
	SuperUsers     []int64  `json:"super_users"`
	RingLen        uint     `json:"ring_len"`
	Latency        int64    `json:"latency"`
	MaxProcessTime int64    `json:"max_process_time"`
}

// WSConfig WebSocket 连接配置
type WSConfig struct {
	Url         string `json:"url"`
	AccessToken string `json:"access_token"`
}

// WallConfig 表白墙配置
type WallConfig struct {
	ShowAuthor   bool     `json:"show_author"`
	AnonDefault  bool     `json:"anon_default"`
	MaxImages    int      `json:"max_images"`
	MaxTextLen   int      `json:"max_text_len"`
	PublishDelay Duration `json:"publish_delay"`
}

// DatabaseConfig 数据库配置
type DatabaseConfig struct {
	Path string `json:"path"`
}

// WebConfig 网页配置
type WebConfig struct {
	Enable bool   `json:"enable"`
	Addr   string `json:"-"`
}

// CensorConfig 敏感词过滤配置
type CensorConfig struct {
	Enable    bool     `json:"enable"`
	Words     []string `json:"words"`
	WordsFile string   `json:"words_file"`
}

// WorkerConfig 任务调度配置
type WorkerConfig struct {
	Workers      int      `json:"workers"`
	RetryCount   int      `json:"retry_count"`
	RetryDelay   Duration `json:"retry_delay"`
	RateLimit    Duration `json:"rate_limit"`
	PollInterval Duration `json:"poll_interval"`
}

// LogConfig 日志配置
type LogConfig struct {
	Level string `json:"level"`
}

// Load 加载 JSON 配置文件
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	cfg := &Config{}
	if err = json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.setDefaults()
	return cfg, nil
}

// Save 将配置序列化为 JSON 写入文件
func (c *Config) Save(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write config file: %w", err)
	}
	return nil
}

func (c *Config) setDefaults() {
	if c.Qzone.KeepAlive.Duration == 0 {
		c.Qzone.KeepAlive.Duration = 30 * time.Minute
	}
	if c.Qzone.MaxRetry == 0 {
		c.Qzone.MaxRetry = 2
	}
	if c.Qzone.Timeout.Duration == 0 {
		c.Qzone.Timeout.Duration = 30 * time.Second
	}
	if c.Bot.Zero.CommandPrefix == "" {
		c.Bot.Zero.CommandPrefix = "/"
	}
	if c.Bot.Zero.NickName == nil {
		c.Bot.Zero.NickName = []string{"表白墙Bot"}
	}
	if c.Wall.MaxImages == 0 {
		c.Wall.MaxImages = 9
	}
	if c.Wall.MaxTextLen == 0 {
		c.Wall.MaxTextLen = 2000
	}
	if c.Database.Path == "" {
		c.Database.Path = "data/data.db"
	}
	if c.Worker.Workers == 0 {
		c.Worker.Workers = 1
	}
	if c.Worker.RetryCount == 0 {
		c.Worker.RetryCount = 3
	}
	if c.Worker.RetryDelay.Duration == 0 {
		c.Worker.RetryDelay.Duration = 5 * time.Second
	}
	if c.Worker.RateLimit.Duration == 0 {
		c.Worker.RateLimit.Duration = 30 * time.Second
	}
	if c.Worker.PollInterval.Duration == 0 {
		c.Worker.PollInterval.Duration = 5 * time.Second
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
}
