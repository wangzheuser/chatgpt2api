package register

// register 包：把 dev 分支（Python 1.8.0）的注册机移植到 Go 版。
// 设计目标是最小侵入：只新增文件，运行状态写入 data/register.json，
// 不修改 upstream 的 config.Config / repository 模型 / 既有 handler。

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// MailProviderConfig 是单个邮箱渠道的配置项，字段与 dev 的 mail.providers 条目对齐。
type MailProviderConfig struct {
	Type      string   `json:"type"`
	Enable    bool     `json:"enable"`
	Domain    []string `json:"domain"`
	Mailboxes string   `json:"mailboxes"`
	BaseURL   string   `json:"base_url,omitempty"`
	APIKey    string   `json:"api_key,omitempty"`
	// 以下为只读展示字段，由服务端在快照里填充，不参与持久化。
	MailboxesCount   int            `json:"mailboxes_count,omitempty"`
	MailboxesPreview []string       `json:"mailboxes_preview,omitempty"`
	MailboxesStats   map[string]int `json:"mailboxes_stats,omitempty"`
}

// MailConfig 对应 dev 的 mail 配置段。
type MailConfig struct {
	RequestTimeout      int                  `json:"request_timeout"`
	WaitTimeout         int                  `json:"wait_timeout"`
	WaitInterval        int                  `json:"wait_interval"`
	APIUseRegisterProxy bool                 `json:"api_use_register_proxy"`
	Providers           []MailProviderConfig `json:"providers"`
}

// MailStats 是渠道与域名维度的成功率统计。
type MailStats struct {
	ProviderStats map[string]BucketStats `json:"mail_provider_stats"`
	DomainStats   map[string]BucketStats `json:"mail_domain_stats"`
}

// BucketStats 是单个桶的成功/失败计数。
type BucketStats struct {
	Success int    `json:"success"`
	Fail    int    `json:"fail"`
	LastErr string `json:"last_error,omitempty"`
}

// Stats 是任务运行统计，字段与 dev 的 stats 对齐。
type Stats struct {
	JobID             string                 `json:"job_id,omitempty"`
	Success           int                    `json:"success"`
	Fail              int                    `json:"fail"`
	Done              int                    `json:"done"`
	Running           int                    `json:"running"`
	Threads           int                    `json:"threads"`
	ElapsedSeconds    float64                `json:"elapsed_seconds"`
	AvgSeconds        float64                `json:"avg_seconds"`
	SuccessRate       float64                `json:"success_rate"`
	CurrentQuota      int                    `json:"current_quota"`
	CurrentAvailable  int                    `json:"current_available"`
	MailProviderStats map[string]BucketStats `json:"mail_provider_stats"`
	MailDomainStats   map[string]BucketStats `json:"mail_domain_stats"`
	StartedAt         string                 `json:"started_at,omitempty"`
	UpdatedAt         string                 `json:"updated_at,omitempty"`
	FinishedAt        string                 `json:"finished_at,omitempty"`
}

// LogEntry 是运行日志条目，只保留最近 300 条。
type LogEntry struct {
	Time  string `json:"time"`
	Text  string `json:"text"`
	Level string `json:"level"`
}

// Config 是持久化到 data/register.json 的注册机配置。
type Config struct {
	Mail  MailConfig `json:"mail"`
	Proxy string     `json:"proxy"`
	// Proxies 为批量注册出口代理，非空时优先于 Proxy 并轮询使用。
	Proxies         []string `json:"proxies"`
	Total           int      `json:"total"`
	Threads         int      `json:"threads"`
	Mode            string   `json:"mode"`
	TargetQuota     int      `json:"target_quota"`
	TargetAvailable int      `json:"target_available"`
	CheckInterval   int      `json:"check_interval"`
	Enabled         bool     `json:"enabled"`
	Stats           Stats    `json:"stats"`
}

const (
	// ModeTotal 表示按提交任务数停止。
	ModeTotal = "total"
	// ModeQuota 表示按号池剩余额度停止。
	ModeQuota = "quota"
	// ModeAvailable 表示按号池可用账号数停止。
	ModeAvailable = "available"

	defaultProvider = "dropmail"
)

var defaultProviderDomains = map[string][]string{
	"dropmail":  {"mail2me.co", "pickmemail.com", "maximail.vip", "mailtowin.com", "pickmail.org"},
	"openinbox": {},
}

// DefaultConfig 返回内置默认配置。
func DefaultConfig() Config {
	return Config{
		Mail: MailConfig{
			RequestTimeout:      30,
			WaitTimeout:         120,
			WaitInterval:        3,
			APIUseRegisterProxy: true,
			Providers: []MailProviderConfig{
				{Type: "dropmail", Enable: true, Domain: append([]string(nil), defaultProviderDomains["dropmail"]...)},
				{Type: "openinbox", Enable: true, Domain: []string{}},
			},
		},
		Proxy:           "",
		Total:           10,
		Threads:         3,
		Mode:            ModeTotal,
		TargetQuota:     100,
		TargetAvailable: 10,
		CheckInterval:   5,
		Enabled:         false,
		Stats:           defaultStats(3),
	}
}

func defaultStats(threads int) Stats {
	return Stats{
		Threads:           threads,
		MailProviderStats: map[string]BucketStats{},
		MailDomainStats:   map[string]BucketStats{},
	}
}

func safeBool(value any, fallback bool) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case nil:
		return fallback
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return fallback
}

// normalizeProxyList 去空、去重并保持顺序。
func normalizeProxyList(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, item := range values {
		value := strings.TrimSpace(item)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func normalizeMode(value string) string {
	switch strings.TrimSpace(value) {
	case ModeQuota:
		return ModeQuota
	case ModeAvailable:
		return ModeAvailable
	default:
		return ModeTotal
	}
}

func (c *Config) normalize() {
	base := DefaultConfig()
	if c.Mail.Providers == nil {
		c.Mail.Providers = base.Mail.Providers
	}
	if c.Mail.RequestTimeout <= 0 {
		c.Mail.RequestTimeout = base.Mail.RequestTimeout
	}
	if c.Mail.WaitTimeout <= 0 {
		c.Mail.WaitTimeout = base.Mail.WaitTimeout
	}
	if c.Mail.WaitInterval <= 0 {
		c.Mail.WaitInterval = base.Mail.WaitInterval
	}
	if c.Total < 1 {
		c.Total = 1
	}
	if c.Threads < 1 {
		c.Threads = 1
	}
	if c.CheckInterval < 1 {
		c.CheckInterval = base.CheckInterval
	}
	if c.TargetQuota < 1 {
		c.TargetQuota = base.TargetQuota
	}
	if c.TargetAvailable < 1 {
		c.TargetAvailable = base.TargetAvailable
	}
	c.Mode = normalizeMode(c.Mode)
	if c.Proxy == "" {
		c.Proxy = base.Proxy
	}
	c.Proxies = normalizeProxyList(c.Proxies)
	c.Proxy = strings.TrimSpace(c.Proxy)
	stats := c.Stats
	if stats.Threads < 1 {
		stats.Threads = c.Threads
	}
	if stats.MailProviderStats == nil {
		stats.MailProviderStats = map[string]BucketStats{}
	}
	if stats.MailDomainStats == nil {
		stats.MailDomainStats = map[string]BucketStats{}
	}
	stats.Threads = c.Threads
	c.Stats = stats
}

// store 负责 data/register.json 的原子读写。
type store struct {
	path string
	mu   sync.Mutex
}

func newStore(dataDir string) *store {
	if dataDir == "" {
		dataDir = "data"
	}
	return &store{path: filepath.Join(dataDir, "register.json")}
}

func (s *store) load() Config {
	cfg := DefaultConfig()
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return cfg
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return DefaultConfig()
	}
	cfg.normalize()
	return cfg
}

func (s *store) save(cfg Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

var errNoProvider = errors.New("mail.providers 没有启用的 provider")
