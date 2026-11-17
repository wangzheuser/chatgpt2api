package register

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Mailbox 是一次注册任务使用的临时邮箱，字段随渠道不同而不同。
type Mailbox struct {
	Provider     string
	ProviderRef  string
	Address      string
	Label        string
	SessionID    string
	Token        string
	InboxID      string
	ClientID     string
	RefreshToken string
	// Seen 记录已扫描过的邮件引用，outlook 渠道用它跳过广告/安全提醒，只在共享 map 上读写。
	Seen          map[string]bool
	CodeNotBefore time.Time
}

// mailMessage 是渠道返回的邮件摘要。
type mailMessage struct {
	MessageID  string
	Subject    string
	Sender     string
	Text       string
	HTML       string
	ReceivedAt time.Time
}

// mailProvider 是邮箱渠道接口，对应 dev 的 BaseMailProvider。
type mailProvider interface {
	name() string
	create(ctx context.Context, username string) (Mailbox, error)
	fetchLatest(ctx context.Context, mailbox Mailbox) (mailMessage, bool, error)
	close()
}

// mailFactory 依据渠道配置构造 provider。
type mailFactory func(entry MailProviderConfig, conf MailConfig, proxy string) mailProvider

var mailRegistry = map[string]mailFactory{
	"dropmail":      newDropMailProvider,
	"openinbox":     newOpenInboxProvider,
	"outlook_token": newOutlookTokenProvider,
	"freecustom":    newFreeCustomProvider,
	"mailtm":        newMailTmProvider,
	"mailgw":        newMailGwProvider,
}

// mailHub 负责渠道选择、域名轮询与状态统计。
type mailHub struct {
	conf          MailConfig
	proxy         string
	recorder      *recorder
	mu            sync.Mutex
	providerIndex int
	domainIndex   int
	providers     []mailProvider
}

func newMailHub(conf MailConfig, proxy string, recorder *recorder) *mailHub {
	return &mailHub{conf: conf, proxy: proxy, recorder: recorder}
}

func (h *mailHub) close() {
	for _, item := range h.providers {
		item.close()
	}
	h.providers = nil
}

func (h *mailHub) entries() []MailProviderConfig {
	result := make([]MailProviderConfig, 0, len(h.conf.Providers))
	for index, item := range h.conf.Providers {
		entry := item
		if entry.Type == "" {
			continue
		}
		entry.Type = strings.ToLower(strings.TrimSpace(entry.Type))
		_ = index
		result = append(result, entry)
	}
	return result
}

func (h *mailHub) enabledEntries() ([]MailProviderConfig, error) {
	items := make([]MailProviderConfig, 0, len(h.conf.Providers))
	for _, item := range h.entries() {
		if item.Enable {
			items = append(items, item)
		}
	}
	if len(items) == 0 {
		return nil, errNoProvider
	}
	return items, nil
}

// nextEntry 在启用的渠道之间轮询，对应 dev 的 _next_entry。
func (h *mailHub) nextEntry() (MailProviderConfig, error) {
	items, err := h.enabledEntries()
	if err != nil {
		return MailProviderConfig{}, err
	}
	if len(items) == 1 {
		return items[0], nil
	}
	h.mu.Lock()
	index := h.providerIndex % len(items)
	h.providerIndex = (h.providerIndex + 1) % len(items)
	h.mu.Unlock()
	return items[index], nil
}

// nextDomain 在渠道域名池内轮询，对应 dev 的 _next_domain。
func (h *mailHub) nextDomain(entry MailProviderConfig) string {
	domains := make([]string, 0, len(entry.Domain))
	for _, item := range entry.Domain {
		if value := strings.TrimSpace(item); value != "" {
			domains = append(domains, value)
		}
	}
	if len(domains) == 0 {
		domains = defaultProviderDomains[entry.Type]
	}
	if len(domains) == 0 {
		return ""
	}
	if len(domains) == 1 {
		return domains[0]
	}
	h.mu.Lock()
	value := domains[h.domainIndex%len(domains)]
	h.domainIndex = (h.domainIndex + 1) % len(domains)
	h.mu.Unlock()
	return value
}

func (h *mailHub) build(entry MailProviderConfig) (mailProvider, error) {
	factory, ok := mailRegistry[entry.Type]
	if !ok {
		return nil, fmt.Errorf("不支持的邮箱渠道: %s", entry.Type)
	}
	return factory(entry, h.conf, h.proxy), nil
}

// create 遍历启用渠道尝试创建邮箱，成功即返回。
func (h *mailHub) create(ctx context.Context, username string) (Mailbox, error) {
	entries, err := h.enabledEntries()
	if err != nil {
		return Mailbox{}, err
	}
	var lastErr error
	for index := 0; index < len(entries); index++ {
		entry, pickErr := h.nextEntry()
		if pickErr != nil {
			return Mailbox{}, pickErr
		}
		provider, buildErr := h.build(entry)
		if buildErr != nil {
			lastErr = buildErr
			continue
		}
		h.mu.Lock()
		h.providers = append(h.providers, provider)
		h.mu.Unlock()
		mailbox, createErr := provider.create(ctx, username)
		if createErr != nil {
			lastErr = createErr
			h.recordResult(provider.name(), strings.TrimSpace(entry.Type), "", false, createErr)
			continue
		}
		mailbox.CodeNotBefore = time.Now().UTC()
		mailbox.Provider = provider.name()
		mailbox.ProviderRef = providerRef(entry, index)
		mailbox.Label = mailbox.Provider + "#" + fmt.Sprint(index+1)
		return mailbox, nil
	}
	if lastErr == nil {
		lastErr = errors.New("所有启用的邮箱提供商均无法创建邮箱")
	}
	return Mailbox{}, lastErr
}

func providerRef(entry MailProviderConfig, index int) string {
	return fmt.Sprintf("%s#%d", entry.Type, index+1)
}

// waitCode 轮询邮箱取验证码，对应 dev 的 provider.wait_for_code。
func (h *mailHub) waitCode(ctx context.Context, mailbox Mailbox) (string, error) {
	entry := MailProviderConfig{Type: mailbox.Provider, Enable: true}
	provider, err := h.build(entry)
	if err != nil {
		return "", err
	}
	defer provider.close()
	deadline := time.Now().Add(time.Duration(h.conf.WaitTimeout) * time.Second)
	interval := time.Duration(h.conf.WaitInterval) * time.Second
	if interval <= 0 {
		interval = 3 * time.Second
	}
	seen := map[string]bool{}
	attempt := 0
	for {
		if time.Now().After(deadline) {
			if h.recorder != nil {
				h.recorder.log(fmt.Sprintf("等待验证码超时：共轮询 %d 次，provider=%s，未收到可用验证码", attempt, mailbox.Provider), "red")
			}
			return "", errors.New("等待注册验证码超时")
		}
		attempt++
		message, ok, fetchErr := provider.fetchLatest(ctx, mailbox)
		if fetchErr != nil {
			h.recorder.log(fmt.Sprintf("第%d次拉取邮件失败: %s", attempt, fetchErr.Error()), "yellow")
		} else if !ok && attempt%5 == 1 {
			h.recorder.log(fmt.Sprintf("第%d次轮询：邮箱暂无邮件", attempt), "")
		} else if ok {
			ref := messageRef(mailbox, message)
			if !seen[ref] && !message.Before(mailbox.CodeNotBefore) {
				if code := extractCode(message); code != "" {
					h.recordResult(mailbox.Provider, mailbox.ProviderRef, domainOf(mailbox.Address), true, nil)
					return code, nil
				}
			}
			seen[ref] = true
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (h *mailHub) recordResult(providerName, providerRef, domain string, success bool, err error) {
	if h.recorder == nil {
		return
	}
	key := providerRef
	if key == "" {
		key = providerName
	}
	h.recorder.bucket("mail_provider_stats", key, success, err)
	if domain != "" {
		h.recorder.bucket("mail_domain_stats", domain, success, err)
	}
}

func domainOf(address string) string {
	if _, domain, found := strings.Cut(address, "@"); found {
		return strings.ToLower(strings.TrimSpace(domain))
	}
	return ""
}

// messageRef 生成邮件去重键，对应 dev 的 _message_tracking_ref。
func messageRef(mailbox Mailbox, message mailMessage) string {
	if message.MessageID != "" {
		return "id:" + mailbox.Provider + ":" + mailbox.Address + ":" + message.MessageID
	}
	sum := sha256.Sum256([]byte(message.Subject + "\n" + message.Sender + "\n" + message.Text + "\n" + message.HTML))
	return "content:" + mailbox.Provider + ":" + mailbox.Address + ":" + hex.EncodeToString(sum[:])
}

// Before 判断邮件是否早于验证码时间窗。
func (m mailMessage) Before(boundary time.Time) bool {
	if m.ReceivedAt.IsZero() || boundary.IsZero() {
		return false
	}
	return m.ReceivedAt.Before(boundary)
}

var (
	codeBlockPattern = regexp.MustCompile(`(?i)background-color:\s*#F3F3F3[^>]*>[\s\S]*?(\d{6})[\s\S]*?</p>`)
	codeLabelPattern = regexp.MustCompile(`(?i)(?:Verification code|code is|代码为|验证码)[:\s]*(\d{6})`)
	// RE2 不支持 lookbehind，用 (?:^|[^#&\w]) 前缀替代 dev 的 (?<![#&])。
	codeAnyPattern  = regexp.MustCompile(`>\s*(\d{6})\s*<|(?:^|[^#&\w])(\d{6})\b`)
	codeIgnoreValue = "177010"
)

// extractCode 从邮件正文提取 6 位验证码，规则与 dev 的 _extract_code 一致。
func extractCode(message mailMessage) string {
	content := strings.TrimSpace(message.Subject + "\n" + message.Text + "\n" + message.HTML)
	if content == "" {
		return ""
	}
	if match := codeBlockPattern.FindStringSubmatch(content); match != nil {
		return match[1]
	}
	if match := codeLabelPattern.FindStringSubmatch(content); match != nil && match[1] != codeIgnoreValue {
		return match[1]
	}
	for _, match := range codeAnyPattern.FindAllStringSubmatch(content, -1) {
		value := match[1]
		if value == "" {
			value = match[2]
		}
		if value != "" && value != codeIgnoreValue {
			return value
		}
	}
	return ""
}

// newMailHTTPClient 构造邮箱 API 使用的 HTTP 客户端（可选代理，跳过证书校验）。
func newMailHTTPClient(proxy string, timeoutSeconds int) *http.Client {
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	if trimmed := normalizeProxy(proxy); trimmed != "" {
		if parsed, err := url.Parse(trimmed); err == nil {
			transport.Proxy = http.ProxyURL(parsed)
		}
	}
	if timeoutSeconds <= 0 {
		timeoutSeconds = 30
	}
	return &http.Client{Transport: transport, Timeout: time.Duration(timeoutSeconds) * time.Second}
}

// mailRequest 发起 JSON 请求并返回原始响应体。
func mailRequest(ctx context.Context, client *http.Client, method, target string, headers map[string]string, body any) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = strings.NewReader(string(payload))
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, 0, err
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	if req.Header.Get("user-agent") == "" {
		req.Header.Set("user-agent", registerUserAgent)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return payload, resp.StatusCode, nil
}
