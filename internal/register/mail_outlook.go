package register

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 本文件实现 outlook_token 渠道：用 refresh_token 读取真实 Outlook/Hotmail 邮箱验证码。
// 凭据池格式与 dev 一致：每行 email----password----client_id----refresh_token。

const (
	outlookGraphScope = "offline_access https://graph.microsoft.com/Mail.Read"
	outlookIMAPScope  = "offline_access https://outlook.office365.com/IMAP.AccessAsUser.All"

	outlookInUseStaleSeconds = 3600
	outlookTokenCacheTTL     = 600 * time.Second

	outlookStateUnused       = "unused"
	outlookStateInUse        = "in_use"
	outlookStateUsed         = "used"
	outlookStateTokenInvalid = "token_invalid"
	outlookStateFailed       = "failed"
)

// 端点声明为变量，便于集成测试替换。
var (
	outlookTokenURL  = "https://login.microsoftonline.com/common/oauth2/v2.0/token"
	outlookGraphURL  = "https://graph.microsoft.com/v1.0/me/messages"
	outlookStatePath = filepath.Join("data", "outlook_token_used.json")
)

// OutlookCredential 是一条邮箱池凭据。
type OutlookCredential struct {
	Email        string
	Password     string
	ClientID     string
	RefreshToken string
}

// outlookStateEntry 是单条凭据的池状态。
type outlookStateEntry struct {
	State     string `json:"state"`
	Reason    string `json:"reason,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

var outlookStateMu sync.Mutex

// cleanOutlookValue 去掉 BOM 与多余空白，对应 dev 的 _clean_outlook_value。
func cleanOutlookValue(value string) string {
	replaced := strings.NewReplacer("\ufeff", "", "\u00a0", " ").Replace(value)
	return strings.TrimSpace(replaced)
}

// parseOutlookCredentials 解析邮箱池文本（email----password----client_id----refresh_token）。
func parseOutlookCredentials(text string) []OutlookCredential {
	credentials := make([]OutlookCredential, 0)
	seen := map[string]bool{}
	for _, rawLine := range strings.Split(text, "\n") {
		line := cleanOutlookValue(rawLine)
		if line == "" || !strings.Contains(line, "----") {
			continue
		}
		parts := strings.SplitN(line, "----", 4)
		if len(parts) != 4 {
			continue
		}
		email := cleanOutlookValue(parts[0])
		password := cleanOutlookValue(parts[1])
		clientID := cleanOutlookValue(parts[2])
		refreshToken := cleanOutlookValue(parts[3])
		if !strings.Contains(email, "@") || clientID == "" || refreshToken == "" {
			continue
		}
		key := strings.ToLower(email)
		if seen[key] {
			continue
		}
		seen[key] = true
		credentials = append(credentials, OutlookCredential{
			Email:        email,
			Password:     password,
			ClientID:     clientID,
			RefreshToken: refreshToken,
		})
	}
	return credentials
}

// serializeOutlookCredentials 把凭据列表还原成池文本（用于「清理未使用」后回写配置）。
func serializeOutlookCredentials(credentials []OutlookCredential) string {
	lines := make([]string, 0, len(credentials))
	for _, item := range credentials {
		lines = append(lines, item.Email+"----"+item.Password+"----"+item.ClientID+"----"+item.RefreshToken)
	}
	return strings.Join(lines, "\n")
}

// mergeOutlookCredentials 合并已存池与新导入文本，按邮箱去重，新导入覆盖旧凭据。
func mergeOutlookCredentials(oldText, newText string) string {
	merged := map[string]OutlookCredential{}
	order := make([]string, 0)
	for _, item := range parseOutlookCredentials(oldText) {
		key := strings.ToLower(item.Email)
		if _, exists := merged[key]; !exists {
			order = append(order, key)
		}
		merged[key] = item
	}
	for _, item := range parseOutlookCredentials(newText) {
		key := strings.ToLower(item.Email)
		if _, exists := merged[key]; !exists {
			order = append(order, key)
		}
		merged[key] = item
	}
	result := make([]OutlookCredential, 0, len(order))
	for _, key := range order {
		result = append(result, merged[key])
	}
	return serializeOutlookCredentials(result)
}

// loadOutlookState 读取状态文件，不存在或损坏时返回空表。
func loadOutlookState() map[string]outlookStateEntry {
	outlookStateMu.Lock()
	defer outlookStateMu.Unlock()
	return loadOutlookStateLocked()
}

func loadOutlookStateLocked() map[string]outlookStateEntry {
	state := map[string]outlookStateEntry{}
	payload, err := os.ReadFile(outlookStatePath)
	if err != nil {
		return state
	}
	var raw map[string]outlookStateEntry
	if err := json.Unmarshal(payload, &raw); err != nil {
		return state
	}
	for key, value := range raw {
		email := strings.ToLower(strings.TrimSpace(key))
		if email == "" {
			continue
		}
		if value.State == "" {
			value.State = outlookStateUsed
		}
		state[email] = value
	}
	return state
}

func saveOutlookStateLocked(state map[string]outlookStateEntry) error {
	if err := os.MkdirAll(filepath.Dir(outlookStatePath), 0o755); err != nil {
		return err
	}
	ordered := make(map[string]outlookStateEntry, len(state))
	keys := make([]string, 0, len(state))
	for key := range state {
		keys = append(keys, key)
	}
	sortStrings(keys)
	for _, key := range keys {
		ordered[key] = state[key]
	}
	payload, err := json.MarshalIndent(ordered, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	tmp := outlookStatePath + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, outlookStatePath)
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// outlookEntryAvailable 判断该凭据当前是否可领用（in_use 超过 3600s 视为陈旧可回收）。
func outlookEntryAvailable(entry outlookStateEntry, now time.Time) bool {
	switch entry.State {
	case outlookStateUsed, outlookStateTokenInvalid, outlookStateFailed:
		return false
	case outlookStateInUse:
		updated, err := time.Parse(time.RFC3339, entry.UpdatedAt)
		if err != nil {
			return true
		}
		return now.Sub(updated) >= outlookInUseStaleSeconds*time.Second
	default:
		return true
	}
}

func setOutlookState(email, state, reason string) {
	key := strings.ToLower(strings.TrimSpace(email))
	if key == "" {
		return
	}
	outlookStateMu.Lock()
	defer outlookStateMu.Unlock()
	store := loadOutlookStateLocked()
	store[key] = outlookStateEntry{State: state, Reason: reason, UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
	_ = saveOutlookStateLocked(store)
}

// releaseOutlookState 把 in_use 释放回未使用。
func releaseOutlookState(email string) {
	key := strings.ToLower(strings.TrimSpace(email))
	if key == "" {
		return
	}
	outlookStateMu.Lock()
	defer outlookStateMu.Unlock()
	store := loadOutlookStateLocked()
	if entry, ok := store[key]; ok && entry.State == outlookStateInUse {
		delete(store, key)
		_ = saveOutlookStateLocked(store)
	}
}

// resetOutlookState 重置池状态：all 清空全部；failed 只清 failed/token_invalid/in_use。
func resetOutlookState(scope string) int {
	outlookStateMu.Lock()
	defer outlookStateMu.Unlock()
	store := loadOutlookStateLocked()
	if len(store) == 0 {
		return 0
	}
	if scope == "failed" {
		removed := 0
		for key, entry := range store {
			switch entry.State {
			case outlookStateFailed, outlookStateTokenInvalid, outlookStateInUse:
				delete(store, key)
				removed++
			}
		}
		_ = saveOutlookStateLocked(store)
		return removed
	}
	count := len(store)
	_ = saveOutlookStateLocked(map[string]outlookStateEntry{})
	return count
}

// pruneOutlookUnused 从池中删除从未使用过的凭据，返回保留列表与删除数量。
func pruneOutlookUnused(credentials []OutlookCredential) ([]OutlookCredential, int) {
	store := loadOutlookState()
	kept := make([]OutlookCredential, 0, len(credentials))
	removed := 0
	for _, credential := range credentials {
		entry, exists := store[strings.ToLower(credential.Email)]
		if exists && entry.State != "" {
			kept = append(kept, credential)
			continue
		}
		removed++
	}
	return kept, removed
}

// outlookPoolStats 统计池内各状态数量（unused 由导入池与状态文件对账得出）。
func outlookPoolStats(credentials []OutlookCredential) map[string]int {
	store := loadOutlookState()
	counts := map[string]int{
		outlookStateUnused: 0, outlookStateInUse: 0, outlookStateUsed: 0,
		outlookStateTokenInvalid: 0, outlookStateFailed: 0,
	}
	if len(credentials) > 0 {
		for _, credential := range credentials {
			entry, exists := store[strings.ToLower(credential.Email)]
			if exists {
				if _, known := counts[entry.State]; known {
					counts[entry.State]++
					continue
				}
			}
			counts[outlookStateUnused]++
		}
		return counts
	}
	for _, entry := range store {
		if _, known := counts[entry.State]; known {
			counts[entry.State]++
		}
	}
	return counts
}

// maskOutlookEmail 脱敏邮箱，仅保留前两位与域名。
func maskOutlookEmail(email string) string {
	local, domain, found := strings.Cut(strings.TrimSpace(email), "@")
	if !found {
		return "***"
	}
	if len(local) > 2 {
		local = local[:2] + "***" + local[len(local)-1:]
	} else if len(local) > 0 {
		local = local[:1] + "***"
	} else {
		local = "***"
	}
	return local + "@" + domain
}

// outlookTokenProvider 使用 refresh_token 通过 Microsoft Graph 拉取邮件。
type outlookTokenProvider struct {
	conf   MailConfig
	client *http.Client
	pool   []OutlookCredential
	label  string
	limit  int
	mode   string

	cacheMu sync.Mutex
	cache   map[string]cachedOutlookToken
}

type cachedOutlookToken struct {
	token   string
	expires time.Time
}

func newOutlookTokenProvider(entry MailProviderConfig, conf MailConfig, proxy string) mailProvider {
	limit := 10
	provider := &outlookTokenProvider{
		conf:   conf,
		client: newMailHTTPClient(proxy, conf.RequestTimeout),
		pool:   parseOutlookCredentials(entry.Mailboxes),
		label:  entry.Type,
		limit:  limit,
		mode:   "graph",
		cache:  map[string]cachedOutlookToken{},
	}
	return provider
}

func (p *outlookTokenProvider) name() string { return "outlook_token" }
func (p *outlookTokenProvider) close()       {}

// exchangeToken 用 refresh_token 换 access_token，对应 dev 的 _exchange_refresh_token。
func (p *outlookTokenProvider) exchangeToken(ctx context.Context, credential OutlookCredential, scope string) (string, error) {
	form := url.Values{}
	form.Set("client_id", credential.ClientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", credential.RefreshToken)
	form.Set("scope", scope)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, outlookTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", registerUserAgent)
	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	payload, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return "", readErr
	}
	if resp.StatusCode != 200 {
		return "", &outlookTokenError{message: fmt.Sprintf("OutlookToken 刷新失败: HTTP %d, %s", resp.StatusCode, truncate(string(payload), 300))}
	}
	var parsed struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return "", err
	}
	if strings.TrimSpace(parsed.AccessToken) == "" {
		return "", &outlookTokenError{message: "OutlookToken 刷新响应缺少 access_token"}
	}
	return strings.TrimSpace(parsed.AccessToken), nil
}

// accessToken 带 600 秒缓存，避免轮询时反复换 token 触发限流。
func (p *outlookTokenProvider) accessToken(ctx context.Context, credential OutlookCredential, scope string) (string, error) {
	p.cacheMu.Lock()
	cached, exists := p.cache[scope]
	p.cacheMu.Unlock()
	if exists && time.Now().Before(cached.expires) {
		return cached.token, nil
	}
	token, err := p.exchangeToken(ctx, credential, scope)
	if err != nil {
		return "", err
	}
	p.cacheMu.Lock()
	p.cache[scope] = cachedOutlookToken{token: token, expires: time.Now().Add(outlookTokenCacheTTL)}
	p.cacheMu.Unlock()
	return token, nil
}

// outlookTokenError 标记凭据本身失效（区别于读邮件失败）。
type outlookTokenError struct{ message string }

func (e *outlookTokenError) Error() string { return e.message }

func isOutlookTokenError(err error) bool {
	var target *outlookTokenError
	return errors.As(err, &target)
}

// create 从池中领用一条可用凭据并标记 in_use。
func (p *outlookTokenProvider) create(ctx context.Context, username string) (Mailbox, error) {
	if len(p.pool) == 0 {
		return Mailbox{}, errors.New("OutlookToken 邮箱池为空，请导入 email----password----client_id----refresh_token")
	}
	store := loadOutlookState()
	now := time.Now().UTC()
	var picked *OutlookCredential
	for index := range p.pool {
		credential := p.pool[index]
		entry := store[strings.ToLower(credential.Email)]
		if outlookEntryAvailable(entry, now) {
			picked = &credential
			break
		}
	}
	if picked == nil {
		stats := outlookPoolStats(p.pool)
		return Mailbox{}, fmt.Errorf("OutlookToken 邮箱池暂无可用邮箱（共 %d 个，unused=%d in_use=%d used=%d invalid=%d failed=%d），请导入新邮箱或重置池状态",
			len(p.pool), stats[outlookStateUnused], stats[outlookStateInUse], stats[outlookStateUsed],
			stats[outlookStateTokenInvalid], stats[outlookStateFailed])
	}
	setOutlookState(picked.Email, outlookStateInUse, "")
	return Mailbox{
		Provider:     "outlook_token",
		Address:      picked.Email,
		ClientID:     picked.ClientID,
		RefreshToken: picked.RefreshToken,
	}, nil
}

// graphMessage 是 Microsoft Graph 的消息结构（只取需要的字段）。
type graphMessage struct {
	ID               string `json:"id"`
	Subject          string `json:"subject"`
	ReceivedDateTime string `json:"receivedDateTime"`
	BodyPreview      string `json:"bodyPreview"`
	From             struct {
		EmailAddress struct {
			Address string `json:"address"`
			Name    string `json:"name"`
		} `json:"emailAddress"`
	} `json:"from"`
	Body struct {
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
	} `json:"body"`
}

// fetchRecent 拉取最近 N 封邮件（最新在前）。
func (p *outlookTokenProvider) fetchRecent(ctx context.Context, mailbox Mailbox) ([]mailMessage, error) {
	credential := OutlookCredential{Email: mailbox.Address, ClientID: mailbox.ClientID, RefreshToken: mailbox.RefreshToken}
	token, err := p.accessToken(ctx, credential, outlookGraphScope)
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	query.Set("$top", fmt.Sprintf("%d", p.limit))
	query.Set("$orderby", "receivedDateTime desc")
	query.Set("$select", "id,subject,receivedDateTime,from,body,bodyPreview")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, outlookGraphURL+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", registerUserAgent)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	payload, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if readErr != nil {
		return nil, readErr
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("OutlookToken Graph 失败: HTTP %d, %s", resp.StatusCode, truncate(string(payload), 300))
	}
	var parsed struct {
		Value []graphMessage `json:"value"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, err
	}
	messages := make([]mailMessage, 0, len(parsed.Value))
	for _, item := range parsed.Value {
		text := item.Body.Content
		html := ""
		if strings.EqualFold(item.Body.ContentType, "html") {
			html = item.Body.Content
			text = item.BodyPreview
		}
		sender := item.From.EmailAddress.Address
		if sender == "" {
			sender = item.From.EmailAddress.Name
		}
		messages = append(messages, mailMessage{
			MessageID:  item.ID,
			Subject:    item.Subject,
			Sender:     sender,
			Text:       text,
			HTML:       html,
			ReceivedAt: parseTimeValue(item.ReceivedDateTime),
		})
	}
	return messages, nil
}

// fetchLatest 返回最近一封未扫描且含验证码的邮件；若都没有则返回最新一封。
func (p *outlookTokenProvider) fetchLatest(ctx context.Context, mailbox Mailbox) (mailMessage, bool, error) {
	messages, err := p.fetchRecent(ctx, mailbox)
	if err != nil {
		return mailMessage{}, false, err
	}
	if len(messages) == 0 {
		return mailMessage{}, false, nil
	}
	for _, message := range messages {
		if message.Before(mailbox.CodeNotBefore) {
			continue
		}
		ref := messageRef(mailbox, message)
		if mailbox.Seen != nil && mailbox.Seen[ref] {
			continue
		}
		if extractCode(message) != "" {
			return message, true, nil
		}
		if mailbox.Seen != nil {
			mailbox.Seen[ref] = true
		}
	}
	return messages[0], true, nil
}
