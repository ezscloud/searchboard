// Package config searchboard 配置（YAML 为唯一配置形态）。
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration 支持 "500ms" / "2s" 等 time.Duration 字面量的 YAML 类型。
type Duration time.Duration

// UnmarshalYAML 解析 duration 字符串。
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("config: invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// D 还原为 time.Duration。
func (d Duration) D() time.Duration { return time.Duration(d) }

// Config 总配置。
type Config struct {
	Listen        string      `yaml:"listen"`        // 如 "127.0.0.1:9201"
	Transport     string      `yaml:"transport"`     // auto / netpoll / std
	Elasticsearch ESConfig    `yaml:"elasticsearch"` // ES/OpenSearch 连接
	Logs          LogConfig   `yaml:"logs"`          // 节点日志追踪
	Brand         BrandConfig `yaml:"brand"`         // 品牌定制（可被运行时 API 覆盖）
	PublicDir     string      `yaml:"public_dir"`    // 外部覆盖目录（logo 等静态资源）；空=仅 embed 基座
}

// ESConfig Elasticsearch / OpenSearch 连接配置。
type ESConfig struct {
	URL                string `yaml:"url"`                  // 如 http://127.0.0.1:9200
	Username           string `yaml:"username"`             // BasicAuth 用户名（可空）
	Password           string `yaml:"password"`             // BasicAuth 密码（可空）
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"` // 跳过 https 证书校验
}

// LogConfig 节点日志追踪配置。
type LogConfig struct {
	Dir            string   `yaml:"dir"`              // 日志目录
	Pattern        string   `yaml:"pattern"`          // 文件名 glob，如 "*.log" / "gc.log*"
	PollInterval   Duration `yaml:"poll_interval"`    // 轮询间隔，默认 500ms
	MaxReplayLines int      `yaml:"max_replay_lines"` // SSE 建立时回放尾部行数，默认 300
	MaxFiles       int      `yaml:"max_files"`        // 文件列表上限，默认 200
}

// BrandConfig 品牌定制。
type BrandConfig struct {
	Name   string `yaml:"name"`   // 站点名
	Slogan string `yaml:"slogan"` // 一句话标语
	Logo   string `yaml:"logo"`   // logo 地址（空=内置默认）；推荐放 <public_dir>/logo.png 覆盖
}

// Load 读取并校验配置（缺省值兜底）。
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:9201"
	}
	if cfg.Transport == "" {
		cfg.Transport = "auto"
	}
	if cfg.Elasticsearch.URL == "" {
		cfg.Elasticsearch.URL = "http://127.0.0.1:9200"
	}
	if cfg.Logs.PollInterval == 0 {
		cfg.Logs.PollInterval = Duration(500 * time.Millisecond)
	}
	if cfg.Logs.MaxReplayLines == 0 {
		cfg.Logs.MaxReplayLines = 300
	}
	if cfg.Logs.MaxFiles == 0 {
		cfg.Logs.MaxFiles = 200
	}
	if cfg.Logs.Pattern == "" {
		cfg.Logs.Pattern = "*.log"
	}
	if cfg.Brand.Name == "" {
		cfg.Brand.Name = "SearchBoard"
	}
	return &cfg, nil
}
