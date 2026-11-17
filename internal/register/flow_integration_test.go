package register

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// TestFlowHappyPath 用本地 httptest 模拟 auth.openai.com 全链路，验证状态机与 cookie 传递。
// 这是 E2E 无法进行时的替代验证：不依赖外网。
func TestFlowHappyPath(t *testing.T) {
	var sawDeviceCookie, cookiePersisted atomic.Bool
	var validateCode atomic.Value

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	oldAuth, oldPlatform, oldFrame, oldReq := authBase, platformBase, sentinelFrameURL, sentinelRequestURL
	authBase, platformBase = srv.URL, srv.URL
	sentinelFrameURL = srv.URL + "/sentinel-frame-missing"
	sentinelRequestURL = srv.URL + "/sentinel-req-missing"
	defer func() {
		authBase, platformBase = oldAuth, oldPlatform
		sentinelFrameURL, sentinelRequestURL = oldFrame, oldReq
	}()

	mux.HandleFunc("/api/accounts/authorize", func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie("oai-did"); err == nil && cookie.Value != "" {
			sawDeviceCookie.Store(true)
		}
		for _, name := range []string{"login_hint", "code_challenge", "state", "nonce", "auth0Client"} {
			if r.URL.Query().Get(name) == "" {
				t.Errorf("authorize missing query %s", name)
			}
		}
		http.SetCookie(w, &http.Cookie{Name: "signin-session", Value: "sess-1", Path: "/"})
		http.Redirect(w, r, "/email-verification", http.StatusFound)
	})
	mux.HandleFunc("/email-verification", func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie("oai-did"); err == nil && cookie.Value != "" {
			cookiePersisted.Store(true)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>verify</html>"))
	})
	mux.HandleFunc("/api/accounts/email-otp/validate", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		validateCode.Store(body["code"])
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"continue_url":"/continue"}`))
	})
	mux.HandleFunc("/continue", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/api/accounts/create_account", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["name"] == "" || body["birthdate"] == "" {
			t.Errorf("create_account missing identity: %v", body)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"continue_url":"%s/auth/callback?code=authcode123"}`, srv.URL)))
	})
	mux.HandleFunc("/api/accounts/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"access_token":"access-token","refresh_token":"refresh-token","id_token":"id-token"}`))
	})

	sess, err := newSession("", newDeviceID(), nil)
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	defer sess.close()

	mailbox := Mailbox{Provider: "fake", Address: "tester@example.com", CodeNotBefore: time.Now().UTC()}
	flow, err := newFlow(sess, newSentinelRunner("node"), mailbox, "", nil)
	if err != nil {
		t.Fatalf("newFlow: %v", err)
	}
	flow.codeWaiter = func(ctx context.Context, mb Mailbox) (string, error) { return "123456", nil }

	result, err := flow.run(context.Background())
	if err != nil {
		t.Fatalf("flow.run: %v", err)
	}
	if result.AccessToken != "access-token" || result.RefreshToken != "refresh-token" || result.IDToken != "id-token" {
		t.Fatalf("unexpected tokens: %+v", result)
	}
	if result.Email != "tester@example.com" {
		t.Fatalf("unexpected email: %q", result.Email)
	}
	if got, _ := validateCode.Load().(string); got != "123456" {
		t.Fatalf("validate code mismatch: %q", got)
	}
	if !sawDeviceCookie.Load() {
		t.Fatal("authorize did not receive oai-did cookie")
	}
	if !cookiePersisted.Load() {
		t.Fatal("cookie jar lost oai-did after redirect — jar not wired")
	}
}

// TestSessionCookieJar Wraps the regression: tls-client 的 Jar 默认为 nil。
func TestSessionCookieJar(t *testing.T) {
	sess, err := newSession("", newDeviceID(), nil)
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	defer sess.close()
	if sess.client.GetCookieJar() == nil {
		t.Fatal("cookie jar must not be nil")
	}
	sess.setCookie("probe", "value", ".openai.com")
	got := sess.client.GetCookies(mustURL(t, "https://auth.openai.com/"))
	found := false
	for _, cookie := range got {
		if cookie.Name == "probe" && cookie.Value == "value" {
			found = true
		}
	}
	if !found {
		t.Fatalf("cookie not stored, got %d cookies", len(got))
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	return parsed
}

// TestParseTimeValueMillis 覆盖 openinbox 返回的毫秒时间戳解析。
func TestParseTimeValueMillis(t *testing.T) {
	got := parseTimeValue("2026-11-16T08:34:03.067Z")
	if got.IsZero() {
		t.Fatal("millisecond RFC3339 should parse")
	}
	if got.Format("2006-01-02T15:04:05Z") != "2026-11-16T08:34:03Z" {
		t.Fatalf("unexpected time: %s", got)
	}
}
