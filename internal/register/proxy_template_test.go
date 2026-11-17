package register

import (
	"regexp"
	"strings"
	"testing"
)

func TestRenderProxyTemplatePassthrough(t *testing.T) {
	cases := []string{"", "  ", "http://127.0.0.1:7890", "socks5h://user:pass@host:1080"}
	for _, input := range cases {
		got := renderProxyTemplate(input)
		want := strings.TrimSpace(input)
		if got != want {
			t.Fatalf("render(%q)=%q want %q", input, got, want)
		}
	}
}

func TestRenderProxyTemplateSingle(t *testing.T) {
	template := "http://user.{uuid}:secret@gw.example.com:9000"
	got := renderProxyTemplate(template)
	if strings.Contains(got, "{uuid}") {
		t.Fatalf("placeholder not replaced: %q", got)
	}
	if !strings.HasPrefix(got, "http://user.") || !strings.Contains(got, ":secret@gw.example.com:9000") {
		t.Fatalf("unexpected rendering: %q", got)
	}
	sessionPattern := regexp.MustCompile(`user\.([0-9a-f]{32}):`)
	match := sessionPattern.FindStringSubmatch(got)
	if match == nil {
		t.Fatalf("session id should be 32 lowercase hex, got %q", got)
	}
}

func TestRenderProxyTemplateMultipleSameSession(t *testing.T) {
	got := renderProxyTemplate("http://{uuid}:{uuid}@host:9000")
	rest := strings.TrimPrefix(got, "http://")
	userPass, _, ok := strings.Cut(rest, "@")
	if !ok {
		t.Fatalf("unexpected rendering: %q", got)
	}
	left, right, ok := strings.Cut(userPass, ":")
	if !ok || left != right {
		t.Fatalf("all placeholders must share one session id: %q", got)
	}
	if len(left) != 32 {
		t.Fatalf("session id length should be 32, got %d", len(left))
	}
}

func TestRenderProxyTemplateUniqueness(t *testing.T) {
	template := "http://user.{uuid}:pw@host:9000"
	first := renderProxyTemplate(template)
	second := renderProxyTemplate(template)
	if first == second {
		t.Fatalf("each call should produce a new session: %q", first)
	}
}

func TestHasProxyTemplate(t *testing.T) {
	if !hasProxyTemplate("http://u.{uuid}:p@h:1") {
		t.Fatal("should detect placeholder")
	}
	if hasProxyTemplate("http://u:p@h:1") {
		t.Fatal("plain proxy must not match")
	}
}

func TestRenderThenNormalize(t *testing.T) {
	rendered := renderProxyTemplate("socks5://u.{uuid}:p@h:1080")
	normalized := normalizeProxy(rendered)
	if !strings.HasPrefix(normalized, "socks5h://u.") {
		t.Fatalf("normalize should keep session and upgrade scheme: %q", normalized)
	}
	if strings.Contains(normalized, "{uuid}") {
		t.Fatalf("placeholder leaked: %q", normalized)
	}
}

func TestProxyStatusKeepsTemplate(t *testing.T) {
	pool := newProxyPool()
	status := pool.status([]string{"http://user.{uuid}:secret@host:9000"}, "")
	if len(status) != 1 {
		t.Fatalf("unexpected status: %+v", status)
	}
	text, _ := status[0]["proxy"].(string)
	if !strings.Contains(text, "{uuid}") {
		t.Fatalf("status should keep the placeholder visible: %q", text)
	}
	if strings.Contains(text, "secret") {
		t.Fatalf("password must be redacted: %q", text)
	}
}
