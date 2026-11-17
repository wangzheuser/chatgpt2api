package register

import (
	"strings"
	"sync"
	"time"
)

// proxyPool 在多个注册出口代理之间轮询，并对连续失败的代理做冷却。
//
// 设计目标：一批代理里个别 IP 被上游风控时，任务自动落到其它代理上，
// 不必人工剔除；冷却结束后自动恢复参与轮询。

const (
	proxyFailureThreshold = 2
	proxyCooldownDuration = 15 * time.Minute
)

type proxyHealth struct {
	fails         int
	cooldownUntil time.Time
	lastUsedAt    time.Time
}

type proxyPool struct {
	mu     sync.Mutex
	index  int
	health map[string]*proxyHealth
}

func newProxyPool() *proxyPool {
	return &proxyPool{health: map[string]*proxyHealth{}}
}

// candidates 返回本次可选代理：优先批量列表，为空时回落单个 proxy。
func candidates(proxies []string, fallback string) []string {
	result := make([]string, 0, len(proxies)+1)
	for _, item := range proxies {
		if value := strings.TrimSpace(item); value != "" {
			result = append(result, value)
		}
	}
	if len(result) == 0 {
		if value := strings.TrimSpace(fallback); value != "" {
			result = append(result, value)
		}
	}
	return result
}

// pick 轮询选择一个可用代理；全部冷却时选冷却最早结束的那个，避免整体停摆。
func (p *proxyPool) pick(proxies []string, fallback string) string {
	if p == nil {
		return ""
	}
	list := candidates(proxies, fallback)
	if len(list) == 0 {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	start := p.index
	for offset := 0; offset < len(list); offset++ {
		index := (start + offset) % len(list)
		candidate := list[index]
		entry := p.health[candidate]
		if entry == nil || entry.cooldownUntil.IsZero() || now.After(entry.cooldownUntil) {
			p.index = (index + 1) % len(list)
			if entry == nil {
				entry = &proxyHealth{}
				p.health[candidate] = entry
			}
			entry.lastUsedAt = now
			return candidate
		}
	}
	// 全部冷却：取冷却最早结束的
	best := list[start%len(list)]
	var bestUntil time.Time
	for offset := 0; offset < len(list); offset++ {
		candidate := list[(start+offset)%len(list)]
		entry := p.health[candidate]
		if entry == nil {
			return candidate
		}
		if bestUntil.IsZero() || entry.cooldownUntil.Before(bestUntil) {
			best, bestUntil = candidate, entry.cooldownUntil
		}
	}
	p.index = (start + 1) % len(list)
	return best
}

// markFailure 记录一次代理失败，达到阈值后进入冷却。
func (p *proxyPool) markFailure(proxy string) {
	target := strings.TrimSpace(proxy)
	if target == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	entry := p.health[target]
	if entry == nil {
		entry = &proxyHealth{}
		p.health[target] = entry
	}
	entry.fails++
	if entry.fails >= proxyFailureThreshold {
		entry.cooldownUntil = time.Now().Add(proxyCooldownDuration)
	}
}

// markSuccess 清零失败计数并解除冷却。
func (p *proxyPool) markSuccess(proxy string) {
	target := strings.TrimSpace(proxy)
	if target == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	entry := p.health[target]
	if entry == nil {
		entry = &proxyHealth{}
		p.health[target] = entry
	}
	entry.fails = 0
	entry.cooldownUntil = time.Time{}
}

// status 返回当前代理健康快照，用于前端展示（密码部分已脱敏）。
func (p *proxyPool) status(proxies []string, fallback string) []map[string]any {
	if p == nil {
		return nil
	}
	list := candidates(proxies, fallback)
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	result := make([]map[string]any, 0, len(list))
	for _, item := range list {
		entry := p.health[item]
		state := "ready"
		fails := 0
		cooldown := 0
		if entry != nil {
			fails = entry.fails
			if !entry.cooldownUntil.IsZero() && now.Before(entry.cooldownUntil) {
				state = "cooling"
				cooldown = int(entry.cooldownUntil.Sub(now).Seconds())
			}
		}
		result = append(result, map[string]any{
			"proxy":        redactProxy(item),
			"state":        state,
			"fails":        fails,
			"cooldown_sec": cooldown,
		})
	}
	return result
}

// isProxyFailure 判断错误是否大概率与出口代理有关（用于触发冷却）。
func isProxyFailure(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	for _, marker := range []string{
		"cloudflare", "eof", "connection reset", "connection refused", "timeout",
		"deadline exceeded", "proxy", "no such host", "tls handshake",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// redactProxy 隐藏代理 URL 中的口令，仅保留用户名。
func redactProxy(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	at := strings.LastIndex(trimmed, "@")
	if at < 0 {
		return trimmed
	}
	scheme := ""
	rest := trimmed
	if index := strings.Index(trimmed, "://"); index >= 0 {
		scheme = trimmed[:index+3]
		rest = trimmed[index+3:]
	}
	userInfo := rest[:strings.LastIndex(rest, "@")]
	if colon := strings.Index(userInfo, ":"); colon >= 0 {
		userInfo = userInfo[:colon] + ":***"
	}
	return scheme + userInfo + rest[strings.LastIndex(rest, "@"):]
}
