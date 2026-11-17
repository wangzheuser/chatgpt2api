package register

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// dropMailProvider 对接 dropmail.me GraphQL（dev 实测可注册）。
type dropMailProvider struct {
	entry     MailProviderConfig
	conf      MailConfig
	client    *http.Client
	base      string
	token     string
	preferred []string
}

const dropMailAppKey = "==gNyAjMfRXZyNWZz9FbxhGchJ3Zf1Gd"

func newDropMailProvider(entry MailProviderConfig, conf MailConfig, proxy string) mailProvider {
	preferred := make([]string, 0, len(entry.Domain))
	for _, item := range entry.Domain {
		if value := strings.ToLower(strings.TrimSpace(item)); value != "" {
			preferred = append(preferred, value)
		}
	}
	if len(preferred) == 0 {
		preferred = append(preferred, defaultProviderDomains["dropmail"]...)
	}
	provider := &dropMailProvider{
		entry:     entry,
		conf:      conf,
		client:    newMailHTTPClient(proxy, conf.RequestTimeout),
		base:      "https://dropmail.me/api/graphql",
		preferred: preferred,
	}
	provider.token = dropMailToken()
	return provider
}

func (p *dropMailProvider) name() string { return "dropmail" }
func (p *dropMailProvider) close()       {}

// dropMailToken 复刻 dev 的 token 算法：反转 appKey -> base64 解码 -> FNV-1a 变体。
func dropMailToken() string {
	reversed := reverseString(dropMailAppKey)
	key := ""
	if decoded, err := base64.StdEncoding.DecodeString(reversed); err == nil {
		key = string(decoded)
	}
	now := time.Now().UTC()
	randomPart := fmt.Sprintf("%04d%02d%02d", now.Year(), int(now.Month()), now.Day()) + randomString("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", 16)
	hash := uint32(2166136261)
	for _, ch := range randomPart + key {
		hash ^= uint32(ch)
		hash = (hash + (hash << 1) + (hash << 4) + (hash << 7) + (hash << 8) + (hash << 24)) & 0xFFFFFFFF
	}
	return fmt.Sprintf("website_%s_%x", randomPart, hash)
}

func reverseString(value string) string {
	buf := []rune(value)
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf)
}

func (p *dropMailProvider) graphql(ctx context.Context, token, query string, variables map[string]any) (map[string]any, error) {
	payload := map[string]any{"query": query}
	if variables != nil {
		payload["variables"] = variables
	}
	target := p.base + "/" + url.PathEscape(token)
	body, status, err := mailRequest(ctx, p.client, http.MethodPost, target, map[string]string{
		"Accept":       "application/json",
		"Content-Type": "application/json",
		"Referer":      "https://dropmail.me/",
	}, payload)
	if err != nil {
		return nil, err
	}
	if status/100 != 2 {
		return nil, fmt.Errorf("DropMail GraphQL HTTP %d, body=%s", status, truncate(string(body), 300))
	}
	var parsed struct {
		Data   map[string]any `json:"data"`
		Errors []any          `json:"errors"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	if len(parsed.Errors) > 0 {
		return nil, fmt.Errorf("DropMail GraphQL errors: %v", parsed.Errors)
	}
	if parsed.Data == nil {
		return map[string]any{}, nil
	}
	return parsed.Data, nil
}

// availableDomainIDs 返回服务端可用域名 name->id 映射。
func (p *dropMailProvider) availableDomainIDs(ctx context.Context, token string) (map[string]string, error) {
	data, err := p.graphql(ctx, token, "query Domains { domains(includeBroken:false) { id name } }", nil)
	if err != nil {
		return nil, err
	}
	result := map[string]string{}
	list, _ := data["domains"].([]any)
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(fmt.Sprint(entry["name"])))
		id := strings.TrimSpace(fmt.Sprint(entry["id"]))
		if name != "" && id != "" {
			result[name] = id
		}
	}
	return result, nil
}

func (p *dropMailProvider) create(ctx context.Context, username string) (Mailbox, error) {
	domains, err := p.availableDomainIDs(ctx, p.token)
	if err != nil {
		return Mailbox{}, err
	}
	candidates := make([]string, 0, len(p.preferred))
	for _, name := range p.preferred {
		if id := domains[name]; id != "" {
			candidates = append(candidates, id)
		}
	}
	domainID := ""
	if len(candidates) > 0 {
		domainID = candidates[rand.Intn(len(candidates))]
	} else {
		for _, id := range domains {
			domainID = id
			break
		}
	}
	input := map[string]any{"withAddress": true}
	if domainID != "" {
		input["domainId"] = domainID
	}
	data, err := p.graphql(ctx, p.token,
		"mutation IntroduceSession($input: IntroduceSessionInput) { introduceSession(input: $input) { id addresses { address } } }",
		map[string]any{"input": input})
	if err != nil {
		return Mailbox{}, err
	}
	session, _ := data["introduceSession"].(map[string]any)
	addresses, _ := session["addresses"].([]any)
	address := ""
	if len(addresses) > 0 {
		if first, ok := addresses[0].(map[string]any); ok {
			address = strings.ToLower(strings.TrimSpace(fmt.Sprint(first["address"])))
		}
	}
	if address == "" {
		return Mailbox{}, errors.New("DropMail 未返回邮箱地址")
	}
	return Mailbox{
		Provider:  "dropmail",
		Address:   address,
		SessionID: strings.TrimSpace(fmt.Sprint(session["id"])),
		Token:     p.token,
	}, nil
}

func (p *dropMailProvider) fetchLatest(ctx context.Context, mailbox Mailbox) (mailMessage, bool, error) {
	token := mailbox.Token
	if token == "" {
		token = p.token
	}
	data, err := p.graphql(ctx, token,
		"query Session($id: ID!) { session(id: $id) { mails { id fromAddr headerFrom headerSubject text html receivedAt } } }",
		map[string]any{"id": mailbox.SessionID})
	if err != nil {
		return mailMessage{}, false, err
	}
	session, ok := data["session"].(map[string]any)
	if !ok || session == nil {
		keys := make([]string, 0, len(data))
		for key := range data {
			keys = append(keys, key)
		}
		return mailMessage{}, false, fmt.Errorf("dropmail_session_not_found id=%s keys=%v", mailbox.SessionID, keys)
	}
	mails, _ := session["mails"].([]any)
	if len(mails) == 0 {
		return mailMessage{}, false, nil
	}
	last, ok := mails[len(mails)-1].(map[string]any)
	if !ok {
		return mailMessage{}, false, nil
	}
	return mailMessage{
		MessageID:  strings.TrimSpace(fmt.Sprint(last["id"])),
		Subject:    strings.TrimSpace(fmt.Sprint(last["headerSubject"])),
		Sender:     strings.TrimSpace(fmt.Sprint(last["headerFrom"])),
		Text:       strings.TrimSpace(fmt.Sprint(last["text"])),
		HTML:       strings.TrimSpace(fmt.Sprint(last["html"])),
		ReceivedAt: parseTimeValue(last["receivedAt"]),
	}, true, nil
}

func truncate(value string, limit int) string {
	if limit > 0 && len(value) > limit {
		return value[:limit]
	}
	return value
}

// parseTimeValue 兼容多种时间表示（RFC3339 / 毫秒时间戳）。
func parseTimeValue(value any) time.Time {
	switch typed := value.(type) {
	case string:
		text := strings.TrimSpace(typed)
		if text == "" {
			return time.Time{}
		}
		for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"} {
			if parsed, err := time.Parse(layout, text); err == nil {
				return parsed.UTC()
			}
		}
		return time.Time{}
	case float64:
		seconds := int64(typed)
		if seconds > 1e12 {
			return time.UnixMilli(seconds).UTC()
		}
		return time.Unix(seconds, 0).UTC()
	}
	return time.Time{}
}
