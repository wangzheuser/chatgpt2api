package register

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 测试用凭据（模拟数据，非真实账号）。
const testOutlookPool = "alice@outlook.com----pw1----client-1----refresh-1\n" +
	"bob@outlook.com----pw2----client-2----refresh-2\n" +
	"alice@outlook.com----pw1b----client-1----refresh-1b\n" +
	"bad-line-without-separator\n" +
	"no-at-sign----pw----cid----rt"

func withTempOutlookState(t *testing.T) {
	t.Helper()
	old := outlookStatePath
	outlookStatePath = filepath.Join(t.TempDir(), "outlook_token_used.json")
	t.Cleanup(func() { outlookStatePath = old })
}

func TestParseOutlookCredentials(t *testing.T) {
	pool := parseOutlookCredentials(testOutlookPool)
	if len(pool) != 2 {
		t.Fatalf("expect 2 valid credentials, got %d: %+v", len(pool), pool)
	}
	if pool[0].Email != "alice@outlook.com" || pool[0].ClientID != "client-1" {
		t.Fatalf("unexpected first credential: %+v", pool[0])
	}
	// 重复邮箱保留首次出现（与 dev 的 parse_outlook_credentials 语义一致）
	if pool[0].Password != "pw1" || pool[0].RefreshToken != "refresh-1" {
		t.Fatalf("duplicate should keep first occurrence: %+v", pool[0])
	}
	if got := parseOutlookCredentials(""); len(got) != 0 {
		t.Fatalf("empty pool should parse to nothing, got %d", len(got))
	}
}

func TestMergeOutlookCredentials(t *testing.T) {
	merged := mergeOutlookCredentials("a@x.com----p----c----r", "b@x.com----p2----c2----r2\na@x.com----p3----c----r3")
	credentials := parseOutlookCredentials(merged)
	if len(credentials) != 2 {
		t.Fatalf("expect 2 credentials, got %d", len(credentials))
	}
	overwritten := ""
	for _, item := range credentials {
		if item.Email == "a@x.com" {
			overwritten = item.RefreshToken
		}
	}
	if overwritten != "r3" {
		t.Fatalf("new import should overwrite old, got %q", overwritten)
	}
}

func TestOutlookStateMachine(t *testing.T) {
	withTempOutlookState(t)
	if got := outlookEntryAvailable(outlookStateEntry{}, time.Now()); !got {
		t.Fatal("empty state should be available")
	}
	setOutlookState("A@x.com", outlookStateInUse, "")
	store := loadOutlookState()
	if entry, ok := store["a@x.com"]; !ok || entry.State != outlookStateInUse {
		t.Fatalf("state not stored: %+v", store)
	}
	if outlookEntryAvailable(store["a@x.com"], time.Now()) {
		t.Fatal("fresh in_use must not be available")
	}
	// 陈旧 in_use 可回收
	if !outlookEntryAvailable(store["a@x.com"], time.Now().Add(2*time.Hour)) {
		t.Fatal("stale in_use should be reclaimable")
	}
	// 终态不可用
	for _, state := range []string{outlookStateUsed, outlookStateFailed, outlookStateTokenInvalid} {
		setOutlookState("a@x.com", state, "reason")
		entry := loadOutlookState()["a@x.com"]
		if outlookEntryAvailable(entry, time.Now().Add(10*time.Hour)) {
			t.Fatalf("state %s must stay unavailable", state)
		}
	}
	// 释放 in_use
	setOutlookState("a@x.com", outlookStateInUse, "")
	releaseOutlookState("a@x.com")
	if _, ok := loadOutlookState()["a@x.com"]; ok {
		t.Fatal("release should delete in_use entry")
	}
	// used 不会被 release 清掉
	setOutlookState("a@x.com", outlookStateUsed, "")
	releaseOutlookState("a@x.com")
	if loadOutlookState()["a@x.com"].State != outlookStateUsed {
		t.Fatal("release must not clear used state")
	}
}

func TestOutlookResetScopes(t *testing.T) {
	withTempOutlookState(t)
	setOutlookState("used@x.com", outlookStateUsed, "")
	setOutlookState("failed@x.com", outlookStateFailed, "boom")
	setOutlookState("busy@x.com", outlookStateInUse, "")

	if cleared := resetOutlookState("failed"); cleared != 2 {
		t.Fatalf("failed scope should clear 2 entries, got %d", cleared)
	}
	store := loadOutlookState()
	if _, ok := store["used@x.com"]; !ok {
		t.Fatal("used entry must survive failed scope")
	}
	if cleared := resetOutlookState("all"); cleared != 1 {
		t.Fatalf("all scope should clear remaining 1 entry, got %d", cleared)
	}
	if len(loadOutlookState()) != 0 {
		t.Fatal("state should be empty after all scope")
	}
}

func TestPruneOutlookUnused(t *testing.T) {
	withTempOutlookState(t)
	credentials := parseOutlookCredentials(testOutlookPool)
	setOutlookState("alice@outlook.com", outlookStateUsed, "")
	kept, removed := pruneOutlookUnused(credentials)
	if removed != 1 || len(kept) != 1 {
		t.Fatalf("expect kept=1 removed=1, got kept=%d removed=%d", len(kept), removed)
	}
	if kept[0].Email != "alice@outlook.com" {
		t.Fatalf("wrong credential kept: %+v", kept[0])
	}
}

func TestOutlookPoolStats(t *testing.T) {
	withTempOutlookState(t)
	credentials := parseOutlookCredentials(testOutlookPool)
	setOutlookState("alice@outlook.com", outlookStateUsed, "")
	stats := outlookPoolStats(credentials)
	if stats[outlookStateUsed] != 1 || stats[outlookStateUnused] != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestMaskOutlookEmail(t *testing.T) {
	cases := map[string]string{
		"alice@outlook.com": "al***e@outlook.com",
		"ab@outlook.com":    "a***@outlook.com",
		"no-domain":         "***",
		"a@outlook.com":     "a***@outlook.com",
	}
	for input, want := range cases {
		if got := maskOutlookEmail(input); got != want {
			t.Fatalf("mask(%q)=%q want %q", input, got, want)
		}
	}
}

func TestOutlookProviderCreateAndFetch(t *testing.T) {
	withTempOutlookState(t)

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	oldToken, oldGraph := outlookTokenURL, outlookGraphURL
	outlookTokenURL = srv.URL + "/token"
	outlookGraphURL = srv.URL + "/me/messages"
	defer func() { outlookTokenURL, outlookGraphURL = oldToken, oldGraph }()

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("client_id") == "" {
			t.Errorf("unexpected token form: %v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"graph-token"}`))
	})
	mux.HandleFunc("/me/messages", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer graph-token" {
			t.Errorf("unexpected auth header: %q", got)
		}
		if r.URL.Query().Get("$top") == "" {
			t.Errorf("missing $top query")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"value": []any{
			map[string]any{
				"id": "m2", "subject": "OpenAI security alert", "receivedDateTime": "2026-11-17T02:00:00Z",
				"bodyPreview": "new sign-in",
				"body":        map[string]any{"contentType": "text", "content": "new sign-in detected"},
			},
			map[string]any{
				"id": "m1", "subject": "Your OpenAI verification code", "receivedDateTime": "2026-11-17T01:59:00Z",
				"from": map[string]any{"emailAddress": map[string]any{"address": "noreply@tm.openai.com"}},
				"body": map[string]any{"contentType": "html", "content": "<p>Your verification code is: <b>654321</b></p>"},
			},
		}})
	})

	provider := newOutlookTokenProvider(MailProviderConfig{Type: "outlook_token", Enable: true, Mailboxes: testOutlookPool}, DefaultConfig().Mail, "")
	mailbox, err := provider.create(context.Background(), "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if mailbox.Address != "alice@outlook.com" || mailbox.ClientID == "" || mailbox.RefreshToken == "" {
		t.Fatalf("unexpected mailbox: %+v", mailbox)
	}
	if loadOutlookState()["alice@outlook.com"].State != outlookStateInUse {
		t.Fatal("picked credential should be marked in_use")
	}
	mailbox.Seen = map[string]bool{}
	mailbox.CodeNotBefore = time.Now().Add(-24 * time.Hour).UTC()

	message, ok, err := provider.fetchLatest(context.Background(), mailbox)
	if err != nil || !ok {
		t.Fatalf("fetchLatest: ok=%v err=%v", ok, err)
	}
	code := extractCode(message)
	if code != "654321" {
		t.Fatalf("expect code 654321, got %q (subject=%q)", code, message.Subject)
	}

	// 第二次领用应拿到另一条凭据
	mailbox2, err := provider.create(context.Background(), "")
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if mailbox2.Address != "bob@outlook.com" {
		t.Fatalf("expect bob, got %q", mailbox2.Address)
	}
	// 第三次：池已用尽
	if _, err := provider.create(context.Background(), ""); err == nil {
		t.Fatal("third create should fail when pool exhausted")
	}
}

func TestOutlookSnapshotRedaction(t *testing.T) {
	withTempOutlookState(t)
	svc := &Service{store: newStore(t.TempDir())}
	svc.cfg = DefaultConfig()
	svc.cfg.Mail.Providers = []MailProviderConfig{
		{Type: "outlook_token", Enable: true, Mailboxes: testOutlookPool},
	}
	setOutlookState("alice@outlook.com", outlookStateUsed, "")

	snapshot := svc.Snapshot()
	register := snapshot["register"].(map[string]any)
	mail := register["mail"].(MailConfig)
	if len(mail.Providers) != 1 {
		t.Fatalf("expect 1 provider, got %d", len(mail.Providers))
	}
	provider := mail.Providers[0]
	if provider.Mailboxes != "" {
		t.Fatalf("mailboxes must be redacted, got %q", provider.Mailboxes)
	}
	if provider.MailboxesCount != 2 {
		t.Fatalf("expect count 2, got %d", provider.MailboxesCount)
	}
	if len(provider.MailboxesPreview) != 2 || strings.Contains(provider.MailboxesPreview[0], "outlook") == false {
		t.Fatalf("unexpected preview: %+v", provider.MailboxesPreview)
	}
	if provider.MailboxesStats[outlookStateUsed] != 1 {
		t.Fatalf("unexpected stats: %+v", provider.MailboxesStats)
	}
	// 内部配置仍保留明文，供渠道使用
	if svc.cfg.Mail.Providers[0].Mailboxes == "" {
		t.Fatal("internal config must keep plaintext pool")
	}
}
