package register

import (
	"errors"
	"testing"
)

func TestCandidatesFallback(t *testing.T) {
	if got := candidates(nil, "http://a:1"); len(got) != 1 || got[0] != "http://a:1" {
		t.Fatalf("fallback failed: %v", got)
	}
	if got := candidates([]string{" ", "http://b:2", "http://b:2"}, "http://a:1"); len(got) != 2 {
		t.Fatalf("list should win and skip blanks: %v", got)
	}
	if got := candidates(nil, ""); len(got) != 0 {
		t.Fatalf("empty should stay empty: %v", got)
	}
}

func TestProxyPoolRotation(t *testing.T) {
	pool := newProxyPool()
	list := []string{"http://p1:1", "http://p2:2", "http://p3:3"}
	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		seen[pool.pick(list, "")]++
	}
	for _, item := range list {
		if seen[item] != 2 {
			t.Fatalf("expect even rotation, got %v", seen)
		}
	}
}

func TestProxyPoolCooldown(t *testing.T) {
	pool := newProxyPool()
	list := []string{"http://p1:1", "http://p2:2"}
	first := pool.pick(list, "")
	pool.markFailure(first)
	if got := pool.pick(list, ""); got == first {
		t.Fatalf("after one failure rotation should move on, got %s", got)
	}
	// 再失败一次触发冷却
	pool.markFailure(first)
	for i := 0; i < 4; i++ {
		if got := pool.pick(list, ""); got == first {
			t.Fatalf("cooling proxy must be skipped, got %s", got)
		}
	}
	// 冷却中的代理在 status 里应标记 cooling
	status := pool.status(list, "")
	cooling := false
	for _, item := range status {
		if item["state"] == "cooling" {
			cooling = true
		}
	}
	if !cooling {
		t.Fatalf("expect a cooling entry, got %+v", status)
	}
	// 成功会解除冷却
	pool.markSuccess(first)
	found := false
	for i := 0; i < 4; i++ {
		if pool.pick(list, "") == first {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("proxy should return to rotation after success")
	}
}

func TestProxyPoolAllCooling(t *testing.T) {
	pool := newProxyPool()
	list := []string{"http://p1:1", "http://p2:2"}
	for _, item := range list {
		pool.markFailure(item)
		pool.markFailure(item)
	}
	got := pool.pick(list, "")
	if got == "" {
		t.Fatal("must still return a proxy when all are cooling")
	}
}

func TestRedactProxy(t *testing.T) {
	cases := map[string]string{
		"http://user:secret@127.0.0.1:7890": "http://user:***@127.0.0.1:7890",
		"socks5h://u:p@host:1080":           "socks5h://u:***@host:1080",
		"http://127.0.0.1:7890":             "http://127.0.0.1:7890",
		"":                                  "",
	}
	for input, want := range cases {
		if got := redactProxy(input); got != want {
			t.Fatalf("redact(%q)=%q want %q", input, got, want)
		}
	}
}

func TestIsProxyFailure(t *testing.T) {
	yes := []error{
		errors.New("被 Cloudflare 拦截，clearance 刷新失败"),
		errors.New("Post \"https://x\": EOF"),
		errors.New("dial tcp: i/o timeout"),
		errors.New("proxyconnect tcp: connection refused"),
	}
	for _, err := range yes {
		if !isProxyFailure(err) {
			t.Fatalf("should be proxy failure: %v", err)
		}
	}
	no := []error{
		errors.New("create_account_http_400 registration_disallowed"),
		errors.New("unsupported_email"),
	}
	for _, err := range no {
		if isProxyFailure(err) {
			t.Fatalf("should not be proxy failure: %v", err)
		}
	}
}

func TestSnapshotExposesProxies(t *testing.T) {
	svc := &Service{store: newStore(t.TempDir()), pool: newProxyPool()}
	svc.cfg = DefaultConfig()
	svc.cfg.Proxies = []string{"http://p1:1", "http://p2:2"}
	snapshot := svc.Snapshot()
	register := snapshot["register"].(map[string]any)
	proxies, ok := register["proxies"].([]string)
	if !ok || len(proxies) != 2 {
		t.Fatalf("proxies missing in snapshot: %+v", register["proxies"])
	}
	status, ok := register["proxy_status"].([]map[string]any)
	if !ok || len(status) != 2 {
		t.Fatalf("proxy_status missing: %+v", register["proxy_status"])
	}
}
