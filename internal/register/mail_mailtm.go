package register

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// hydraMailProvider 对接 mail.tm / mail.gw 这类 Hydra/JSON-LD 邮箱 API。
type hydraMailProvider struct {
	providerName string
	apiBase      string
	conf         MailConfig
	client       *http.Client
	forced       []string
}

func newMailTmProvider(entry MailProviderConfig, conf MailConfig, proxy string) mailProvider {
	return newHydraProvider("mailtm", "https://api.mail.tm", entry, conf, proxy)
}

func newMailGwProvider(entry MailProviderConfig, conf MailConfig, proxy string) mailProvider {
	return newHydraProvider("mailgw", "https://api.mail.gw", entry, conf, proxy)
}

func newHydraProvider(providerName, defaultBase string, entry MailProviderConfig, conf MailConfig, proxy string) *hydraMailProvider {
	base := strings.TrimRight(strings.TrimSpace(entry.BaseURL), "/")
	if base == "" {
		base = defaultBase
	}
	forced := make([]string, 0, len(entry.Domain))
	for _, item := range entry.Domain {
		if value := strings.ToLower(strings.TrimSpace(item)); value != "" {
			forced = append(forced, value)
		}
	}
	return &hydraMailProvider{
		providerName: providerName,
		apiBase:      base,
		conf:         conf,
		client:       newMailHTTPClient(proxy, conf.RequestTimeout),
		forced:       forced,
	}
}

func (p *hydraMailProvider) name() string { return p.providerName }
func (p *hydraMailProvider) close()       {}

// request 发起 Hydra API 调用，返回单对象与集合两种视图。
func (p *hydraMailProvider) request(ctx context.Context, method, path, token string, payload any) (map[string]any, []map[string]any, error) {
	headers := map[string]string{"Accept": "application/ld+json, application/json, text/plain, */*"}
	if payload != nil {
		headers["Content-Type"] = "application/json"
	}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	body, status, err := mailRequest(ctx, p.client, method, p.apiBase+path, headers, payload)
	if err != nil {
		return nil, nil, err
	}
	if status/100 != 2 {
		return nil, nil, fmt.Errorf("%s 请求失败: %s %s, HTTP %d, body=%s", p.providerName, method, path, status, truncate(string(body), 300))
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return map[string]any{}, nil, nil
	}
	var list []map[string]any
	if err := json.Unmarshal(body, &list); err == nil {
		return nil, list, nil
	}
	var object map[string]any
	if err := json.Unmarshal(body, &object); err != nil {
		return nil, nil, err
	}
	var collection struct {
		Members []map[string]any `json:"hydra:member"`
		Data    []map[string]any `json:"data"`
	}
	_ = json.Unmarshal(body, &collection)
	members := collection.Members
	if len(members) == 0 {
		members = collection.Data
	}
	return object, members, nil
}

func (p *hydraMailProvider) domains(ctx context.Context) []string {
	_, list, err := p.request(ctx, http.MethodGet, "/domains?page=1", "", nil)
	if err != nil {
		return nil
	}
	result := make([]string, 0, len(list))
	for _, item := range list {
		if active, ok := item["isActive"].(bool); ok && !active {
			continue
		}
		if domain, ok := item["domain"].(string); ok && strings.TrimSpace(domain) != "" {
			result = append(result, strings.ToLower(strings.TrimSpace(domain)))
		}
	}
	return result
}

func (p *hydraMailProvider) create(ctx context.Context, username string) (Mailbox, error) {
	domains := p.forced
	if len(domains) == 0 {
		domains = p.domains(ctx)
	}
	if len(domains) == 0 {
		return Mailbox{}, fmt.Errorf("%s 没有可用域名", p.providerName)
	}
	lastErr := ""
	for attempt := 0; attempt < 5; attempt++ {
		domain := domains[attempt%len(domains)]
		local := username
		if local == "" {
			local = randomString("abcdefghijklmnopqrstuvwxyz", 5) + fmt.Sprintf("%d", randInt(0, 99))
		}
		address := strings.ToLower(local + "@" + domain)
		password := "Kx!" + randomString("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", 16)
		if _, _, err := p.request(ctx, http.MethodPost, "/accounts", "", map[string]string{"address": address, "password": password}); err != nil {
			lastErr = err.Error()
			continue
		}
		tokenData, _, err := p.request(ctx, http.MethodPost, "/token", "", map[string]string{"address": address, "password": password})
		if err != nil {
			lastErr = err.Error()
			continue
		}
		token, _ := tokenData["token"].(string)
		if strings.TrimSpace(token) == "" {
			lastErr = "token 响应缺少 token"
			continue
		}
		return Mailbox{Provider: p.providerName, Address: address, Token: token}, nil
	}
	if lastErr == "" {
		lastErr = p.providerName + " 邮箱生成失败"
	}
	return Mailbox{}, errors.New(lastErr)
}

func (p *hydraMailProvider) fetchLatest(ctx context.Context, mailbox Mailbox) (mailMessage, bool, error) {
	_, list, err := p.request(ctx, http.MethodGet, "/messages", mailbox.Token, nil)
	if err != nil {
		return mailMessage{}, false, err
	}
	if len(list) == 0 {
		return mailMessage{}, false, nil
	}
	item := list[0]
	messageID, _ := item["id"].(string)
	if messageID == "" {
		if at, ok := item["@id"].(string); ok {
			messageID = strings.Trim(strings.ReplaceAll(at, "/messages/", ""), "/")
		}
	}
	detail := item
	if messageID != "" {
		if full, _, detailErr := p.request(ctx, http.MethodGet, "/messages/"+messageID, mailbox.Token, nil); detailErr == nil && len(full) > 0 {
			detail = full
		}
	}
	text := stringValue(detail["text"])
	html := stringValue(detail["html"])
	if text == "" && html == "" {
		if body, ok := detail["body"].(map[string]any); ok {
			text = stringValue(body["text"])
			html = stringValue(body["html"])
		}
		if text == "" {
			text = stringValue(detail["intro"])
		}
	}
	sender := stringValue(detail["from"])
	if object, ok := detail["from"].(map[string]any); ok {
		sender = stringValue(object["address"])
		if sender == "" {
			sender = stringValue(object["name"])
		}
	}
	return mailMessage{
		MessageID:  messageID,
		Subject:    stringValue(detail["subject"]),
		Sender:     sender,
		Text:       text,
		HTML:       html,
		ReceivedAt: parseTimeValue(firstNonEmptyValue(detail["createdAt"], detail["receivedAt"])),
	}, true, nil
}

// stringValue 把 JSON 值（可能是字符串或字符串数组）转成文本。
func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			parts = append(parts, stringValue(item))
		}
		return strings.Join(parts, "")
	default:
		return ""
	}
}

func firstNonEmptyValue(values ...any) any {
	for _, value := range values {
		if value == nil {
			continue
		}
		if text, ok := value.(string); ok {
			if strings.TrimSpace(text) != "" {
				return text
			}
			continue
		}
		return value
	}
	return nil
}
