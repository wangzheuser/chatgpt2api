package register

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// openInboxProvider 对接 api.openinbox.io（dev 实测可注册）。
type openInboxProvider struct {
	conf   MailConfig
	client *http.Client
	base   string
}

func newOpenInboxProvider(entry MailProviderConfig, conf MailConfig, proxy string) mailProvider {
	return &openInboxProvider{conf: conf, client: newMailHTTPClient(proxy, conf.RequestTimeout), base: "https://api.openinbox.io/api"}
}

func (p *openInboxProvider) name() string { return "openinbox" }
func (p *openInboxProvider) close()       {}

func (p *openInboxProvider) create(ctx context.Context, username string) (Mailbox, error) {
	seed := "kiro" + randomString("abcdefghijklmnopqrstuvwxyz", 5) + time.Now().UTC().Format(time.RFC3339Nano)
	sum := sha256.Sum256([]byte(seed))
	body, status, err := mailRequest(ctx, p.client, http.MethodPost, p.base+"/inbox", map[string]string{
		"Accept":       "application/json,text/plain,*/*",
		"Content-Type": "application/json",
		"Referer":      "https://api.openinbox.io/",
	}, map[string]string{"fingerprint": hex.EncodeToString(sum[:])})
	if err != nil {
		return Mailbox{}, err
	}
	if status/100 != 2 {
		return Mailbox{}, fmt.Errorf("OpenInbox 创建 HTTP %d, body=%s", status, truncate(string(body), 200))
	}
	var parsed struct {
		Email string `json:"email"`
		ID    string `json:"id"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Mailbox{}, err
	}
	address := strings.ToLower(strings.TrimSpace(parsed.Email))
	inboxID := strings.TrimSpace(parsed.ID)
	if !strings.Contains(address, "@") || inboxID == "" {
		return Mailbox{}, errors.New("OpenInbox 未返回 email/id")
	}
	return Mailbox{Provider: "openinbox", Address: address, InboxID: inboxID}, nil
}

func (p *openInboxProvider) fetchLatest(ctx context.Context, mailbox Mailbox) (mailMessage, bool, error) {
	target := p.base + "/emails/inbox/" + url.PathEscape(mailbox.InboxID) + "?page=1&limit=30"
	body, status, err := mailRequest(ctx, p.client, http.MethodGet, target, map[string]string{
		"Accept":  "application/json,text/plain,*/*",
		"Referer": "https://api.openinbox.io/",
	}, nil)
	if err != nil {
		return mailMessage{}, false, err
	}
	if status != 200 {
		return mailMessage{}, false, fmt.Errorf("OpenInbox 列表 HTTP %d", status)
	}
	var listed struct {
		Emails []map[string]any `json:"emails"`
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		return mailMessage{}, false, err
	}
	if len(listed.Emails) == 0 {
		return mailMessage{}, false, nil
	}
	item := listed.Emails[0]
	messageID := ""
	for _, key := range []string{"id", "_id", "message_id", "messageId"} {
		if value, ok := item[key].(string); ok && strings.TrimSpace(value) != "" {
			messageID = strings.TrimSpace(value)
			break
		}
	}
	detail := item
	if messageID != "" {
		detailBody, detailStatus, detailErr := mailRequest(ctx, p.client, http.MethodGet, p.base+"/emails/"+url.PathEscape(messageID), map[string]string{
			"Accept":  "application/json,text/plain,*/*",
			"Referer": "https://api.openinbox.io/",
		}, nil)
		if detailErr == nil && detailStatus == 200 {
			var parsed map[string]any
			if json.Unmarshal(detailBody, &parsed) == nil && parsed != nil {
				detail = parsed
			}
		}
	}
	content := flattenStrings(detail)
	return mailMessage{
		MessageID:  messageID,
		Text:       content,
		ReceivedAt: parseTimeValue(detail["receivedAt"]),
	}, true, nil
}

// flattenStrings 把任意 JSON 结构里的字符串拼接成文本，便于验证码提取。
func flattenStrings(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			parts = append(parts, flattenStrings(item))
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			parts = append(parts, flattenStrings(item))
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}
