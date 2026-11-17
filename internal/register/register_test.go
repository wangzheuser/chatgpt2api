package register

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeFloorsAndMode(t *testing.T) {
	cfg := Config{Total: 0, Threads: 0, Mode: "weird", CheckInterval: 0}
	cfg.normalize()
	if cfg.Total != 1 || cfg.Threads != 1 || cfg.CheckInterval != 5 {
		t.Fatalf("floors not applied: %+v", cfg)
	}
	if cfg.Mode != ModeTotal {
		t.Fatalf("mode should fallback to total, got %q", cfg.Mode)
	}
	if len(cfg.Mail.Providers) == 0 {
		t.Fatal("providers should not be empty after normalize")
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := newStore(dir)
	cfg := DefaultConfig()
	cfg.Threads = 5
	cfg.Mode = ModeQuota
	cfg.TargetQuota = 42
	if err := s.save(cfg); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded := s.load()
	if loaded.Threads != 5 || loaded.Mode != ModeQuota || loaded.TargetQuota != 42 {
		t.Fatalf("round trip mismatch: %+v", loaded)
	}
	if _, err := os.Stat(filepath.Join(dir, "register.json")); err != nil {
		t.Fatalf("file missing: %v", err)
	}
}

func TestPKCEPair(t *testing.T) {
	verifier, challenge := pkcePair()
	if len(verifier) < 40 || len(challenge) < 40 {
		t.Fatalf("pkce too short: %q %q", verifier, challenge)
	}
	_, recomputed := pkcePairFromString(verifier)
	if recomputed != challenge {
		t.Fatalf("challenge mismatch")
	}
}

func TestExtractCode(t *testing.T) {
	cases := []struct {
		name string
		msg  mailMessage
		want string
	}{
		{"label", mailMessage{Text: "Your verification code is: 483920"}, "483920"},
		{"html-block", mailMessage{HTML: `<p style="background-color:#F3F3F3">111222</p>`}, "111222"},
		{"bracket", mailMessage{Text: "code 901234 thanks"}, "901234"},
		{"ignored-default", mailMessage{Text: "code 177010"}, ""},
		{"none", mailMessage{Text: "hello"}, ""},
	}
	for _, item := range cases {
		got := extractCode(item.msg)
		if got != item.want {
			t.Fatalf("%s: got %q want %q", item.name, got, item.want)
		}
	}
}

func TestMailHubRotation(t *testing.T) {
	hub := newMailHub(DefaultConfig().Mail, "", &recorder{svc: nil})
	first, err := hub.nextEntry()
	if err != nil || first.Type == "" {
		t.Fatalf("nextEntry: %v %q", err, first.Type)
	}
	second, _ := hub.nextEntry()
	if first.Type == second.Type {
		t.Fatalf("rotation should alternate, got %q twice", first.Type)
	}
	domain := hub.nextDomain(MailProviderConfig{Type: "dropmail", Domain: []string{"a.com", "b.com"}})
	if domain != "a.com" && domain != "b.com" {
		t.Fatalf("unexpected domain %q", domain)
	}
}

func TestExtractContinueURL(t *testing.T) {
	data := map[string]any{"page": map[string]any{"payload": map[string]any{"continue_url": "https://example.com/x"}}}
	if got := extractContinueURL(data); got != "https://example.com/x" {
		t.Fatalf("got %q", got)
	}
	if got := oauthCallbackCode("https://platform.openai.com/auth/callback?code=abc&state=1"); got != "abc" {
		t.Fatalf("got %q", got)
	}
}

func TestServiceSnapshotShape(t *testing.T) {
	svc := &Service{store: newStore(t.TempDir())}
	svc.cfg = DefaultConfig()
	snapshot := svc.Snapshot()
	register, ok := snapshot["register"].(map[string]any)
	if !ok {
		t.Fatal("missing register key")
	}
	for _, key := range []string{"mail", "proxy", "total", "threads", "mode", "target_quota", "target_available", "check_interval", "enabled", "stats", "logs"} {
		if _, exists := register[key]; !exists {
			t.Fatalf("missing key %q", key)
		}
	}
	_ = context.Background()
}
