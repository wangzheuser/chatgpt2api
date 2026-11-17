package register

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// freeCustomProvider 对接 freecustom.email（零配置，2026-11 探测仍能收到 OpenAI 验证码）。
type freeCustomProvider struct {
	conf     MailConfig
	client   *http.Client
	base     string
	domains  []string
	fallback []string
}

var freeCustomFallbackDomains = []string{
	"ditapi.info", "ditcloud.info", "ditdrive.info", "ditgame.info", "ditlearn.info",
	"ditpay.info", "ditplay.info", "ditube.info", "ditmail.info", "fce.email",
}

func newFreeCustomProvider(entry MailProviderConfig, conf MailConfig, proxy string) mailProvider {
	base := strings.TrimRight(strings.TrimSpace(entry.BaseURL), "/")
	if base == "" {
		base = "https://www.freecustom.email"
	}
	domains := make([]string, 0, len(entry.Domain))
	for _, item := range entry.Domain {
		if value := strings.ToLower(strings.TrimSpace(item)); value != "" {
			domains = append(domains, value)
		}
	}
	return &freeCustomProvider{
		conf:     conf,
		client:   newMailHTTPClient(proxy, conf.RequestTimeout),
		base:     base,
		domains:  domains,
		fallback: append([]string(nil), freeCustomFallbackDomains...),
	}
}

func (p *freeCustomProvider) name() string { return "freecustom" }
func (p *freeCustomProvider) close()       {}

func (p *freeCustomProvider) headers(token string) map[string]string {
	headers := map[string]string{
		"Accept":       "application/json,text/plain,*/*",
		"Referer":      p.base + "/en",
		"x-fce-client": "web-client",
	}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	return headers
}

// call 发起请求并返回解析后的对象；数组响应包成 {"_list": [...]}。
func (p *freeCustomProvider) call(ctx context.Context, method, path, token string) (map[string]any, error) {
	body, status, err := mailRequest(ctx, p.client, method, p.base+path, p.headers(token), nil)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("FreeCustom 请求失败: %s %s, HTTP %d, body=%s", method, path, status, truncate(string(body), 300))
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return map[string]any{}, nil
	}
	var object map[string]any
	if err := json.Unmarshal(body, &object); err == nil && object != nil {
		return object, nil
	}
	var list []any
	if err := json.Unmarshal(body, &list); err == nil {
		return map[string]any{"_list": list}, nil
	}
	return map[string]any{}, nil
}

// domainPool 拉取免费层域名，失败时回落到内置池。
func (p *freeCustomProvider) domainPool(ctx context.Context, token string) []string {
	if len(p.domains) > 0 {
		return p.domains
	}
	data, err := p.call(ctx, http.MethodGet, "/api/domains", token)
	if err != nil {
		return p.fallback
	}
	items, _ := data["data"].([]any)
	result := make([]string, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		tier := strings.ToLower(strings.TrimSpace(fmt.Sprint(entry["tier"])))
		if tier != "" && tier != "free" {
			continue
		}
		if domain := strings.ToLower(strings.TrimSpace(fmt.Sprint(entry["domain"]))); domain != "" {
			result = append(result, domain)
		}
	}
	if len(result) == 0 {
		return p.fallback
	}
	return result
}

func (p *freeCustomProvider) create(ctx context.Context, username string) (Mailbox, error) {
	auth, err := p.call(ctx, http.MethodPost, "/api/auth", "")
	if err != nil {
		return Mailbox{}, err
	}
	token, _ := auth["token"].(string)
	if strings.TrimSpace(token) == "" {
		return Mailbox{}, errors.New("FreeCustom auth 未返回 token")
	}
	domains := p.domainPool(ctx, token)
	local := strings.ToLower(strings.TrimSpace(username))
	if local == "" {
		local = randomString("abcdefghijklmnopqrstuvwxyz", 5) + fmt.Sprintf("%02d", randInt(0, 99))
	}
	local = strings.Trim(local, ".-_")
	if local == "" {
		local = randomString("abcdefghijklmnopqrstuvwxyz", 6)
	}
	domain := domains[randInt(0, len(domains)-1)]
	return Mailbox{Provider: "freecustom", Address: local + "@" + domain, Token: token}, nil
}

func (p *freeCustomProvider) fetchLatest(ctx context.Context, mailbox Mailbox) (mailMessage, bool, error) {
	target := "/api/public-mailbox?fullMailboxId=" + url.QueryEscape(mailbox.Address)
	data, err := p.call(ctx, http.MethodGet, target, mailbox.Token)
	if err != nil {
		return mailMessage{}, false, err
	}
	messages := firstList(data, "messages", "data", "hydra:member", "_list")
	if len(messages) == 0 {
		return mailMessage{}, false, nil
	}
	item := messages[0]
	messageID := firstString(item, "id", "_id", "uid", "messageId")
	detail := item
	if messageID != "" {
		detailTarget := target + "&messageId=" + url.QueryEscape(messageID)
		if full, detailErr := p.call(ctx, http.MethodGet, detailTarget, mailbox.Token); detailErr == nil {
			if nested, ok := full["data"].(map[string]any); ok && len(nested) > 0 {
				detail = nested
			} else if len(full) > 0 {
				detail = full
			}
		}
	}
	text := firstString(detail, "text", "text_content", "body", "intro")
	html := firstString(detail, "html", "html_content")
	if text == "" && html == "" {
		text = flattenStrings(detail)
	}
	return mailMessage{
		MessageID:  messageID,
		Subject:    firstString(detail, "subject", "title"),
		Sender:     firstString(detail, "from", "sender"),
		Text:       text,
		HTML:       html,
		ReceivedAt: parseTimeValue(firstNonEmptyValue(detail["date"], detail["createdAt"], detail["created_at"])),
	}, true, nil
}

// firstList 从对象里按候选键取第一个数组。
func firstList(data map[string]any, keys ...string) []map[string]any {
	for _, key := range keys {
		raw, ok := data[key].([]any)
		if !ok {
			continue
		}
		items := make([]map[string]any, 0, len(raw))
		for _, entry := range raw {
			if object, ok := entry.(map[string]any); ok {
				items = append(items, object)
			}
		}
		if len(items) > 0 {
			return items
		}
	}
	return nil
}

func firstString(data map[string]any, keys ...string) string {
	for _, key := range keys {
		switch value := data[key].(type) {
		case string:
			if strings.TrimSpace(value) != "" {
				return value
			}
		case map[string]any:
			if text := firstString(value, "text", "html", "content", "address", "name"); text != "" {
				return text
			}
		}
	}
	return ""
}
