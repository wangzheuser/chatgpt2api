package register

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// clearanceProvider 是注入的 Cloudflare 清障能力（由 service.ProxyService 适配）。
type clearanceProvider interface {
	Clearance(ctx context.Context, targetURL string) (map[string]string, string, error)
}

// session 封装一个注册任务使用的 HTTP 会话，保持 cookie 与浏览器指纹一致。
type session struct {
	client    tls_client.HttpClient
	proxy     string
	deviceID  string
	clearance clearanceProvider
}

func newSession(proxy, deviceID string, clearance clearanceProvider) (*session, error) {
	options := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(60),
		tls_client.WithClientProfile(profiles.Chrome_146),
		tls_client.WithRandomTLSExtensionOrder(),
		// 必须显式注入 cookie jar：tls-client 默认 c.Jar 为 nil，SetCookies 会静默丢弃，
		// 表现为 authorize 能通过但 validate_otp 返回 409（sign-in session 丢失）。
		tls_client.WithCookieJar(tls_client.NewCookieJar()),
	}
	if trimmed := strings.TrimSpace(proxy); trimmed != "" {
		options = append(options, tls_client.WithProxyUrl(normalizeProxy(trimmed)))
	}
	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), options...)
	if err != nil {
		return nil, err
	}
	return &session{client: client, proxy: proxy, deviceID: deviceID, clearance: clearance}, nil
}

func (s *session) close() {
	if s.client != nil {
		s.client.CloseIdleConnections()
	}
}

// setCookie 把 cookie 写入指定域（模拟 Python 的 cookies.set(domain=...)）。
func (s *session) setCookie(name, value, domain string) {
	host := strings.TrimPrefix(strings.TrimSpace(domain), ".")
	if host == "" {
		return
	}
	target := &url.URL{Scheme: "https", Host: host}
	s.client.SetCookies(target, []*http.Cookie{{
		Name:   name,
		Value:  value,
		Domain: strings.TrimSpace(domain),
		Path:   "/",
	}})
}

// applySentinelCookie 把 oai-sc 写入 openai 四个域。
func (s *session) applySentinelCookie(value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	for _, domain := range []string{".openai.com", "openai.com", ".auth.openai.com", "auth.openai.com"} {
		s.setCookie("oai-sc", value, domain)
	}
}

// applyClearance 把 Cloudflare clearance cookie 注入会话。
func (s *session) applyClearance(ctx context.Context, targetURL string) error {
	if s.clearance == nil {
		return nil
	}
	cookies, _, err := s.clearance.Clearance(ctx, targetURL)
	if err != nil {
		return err
	}
	parsed, parseErr := url.Parse(targetURL)
	if parseErr != nil {
		return parseErr
	}
	host := parsed.Hostname()
	for name, value := range cookies {
		s.setCookie(name, value, "."+host)
		s.setCookie(name, value, host)
	}
	return nil
}

type response struct {
	status int
	body   []byte
	url    string
}

func (r response) json() map[string]any {
	if len(r.body) == 0 {
		return map[string]any{}
	}
	var data map[string]any
	if err := json.Unmarshal(r.body, &data); err != nil {
		return map[string]any{}
	}
	return data
}

func (r response) text() string { return string(r.body) }

// debug 返回截断后的响应描述，供错误信息使用。
func (r response) debug(limit int) string {
	body := strings.TrimSpace(string(r.body))
	if limit > 0 && len(body) > limit {
		body = body[:limit]
	}
	return fmt.Sprintf("status=%d url=%s body=%s", r.status, r.url, body)
}

type requestOptions struct {
	headers map[string]string
	json    any
	form    string
	raw     []byte
	timeout time.Duration
}

// do 是带本地重试的请求入口，network/5xx 错误重试，4xx 直接返回。
func (s *session) do(ctx context.Context, method, target string, opts requestOptions) (response, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		result, err := s.doOnce(ctx, method, target, opts)
		if err == nil {
			if result.status >= 500 && attempt < 2 {
				lastErr = fmt.Errorf("http_%d", result.status)
				time.Sleep(time.Duration(300*(attempt+1)) * time.Millisecond)
				continue
			}
			return result, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return response{}, ctx.Err()
		}
		time.Sleep(time.Duration(300*(attempt+1)) * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = errors.New("request_failed")
	}
	return response{}, lastErr
}

func (s *session) doOnce(ctx context.Context, method, target string, opts requestOptions) (response, error) {
	var body io.Reader
	if opts.json != nil {
		payload, err := json.Marshal(opts.json)
		if err != nil {
			return response{}, err
		}
		body = bytes.NewReader(payload)
	} else if opts.form != "" {
		body = strings.NewReader(opts.form)
	} else if len(opts.raw) > 0 {
		body = bytes.NewReader(opts.raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return response{}, err
	}
	for name, value := range opts.headers {
		req.Header.Set(name, value)
	}
	if opts.headers["user-agent"] == "" {
		req.Header.Set("user-agent", registerUserAgent)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return response{}, err
	}
	defer resp.Body.Close()
	payload, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if readErr != nil {
		return response{}, readErr
	}
	finalURL := target
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	return response{status: resp.StatusCode, body: payload, url: finalURL}, nil
}

// isCloudflareChallenge 判定响应是否为 Cloudflare 挑战页。
func isCloudflareChallenge(resp response) bool {
	if resp.status != 403 && resp.status != 503 && resp.status != 429 {
		return false
	}
	body := strings.ToLower(string(resp.body))
	for _, marker := range []string{"just a moment", "cf-challenge", "cf_chl_opt", "checking your browser", "attention required"} {
		if strings.Contains(body, marker) {
			return true
		}
	}
	return false
}

// cloudflareBlockMessage 生成统一的 Cloudflare 拦截错误。
func cloudflareBlockMessage(resp response, prefix, reason string) string {
	if reason == "" {
		reason = "clearance 刷新失败或重试后仍失败，请更换 IP/代理重试"
	}
	return fmt.Sprintf("%s，%s: %s", prefix, reason, resp.debug(300))
}

// normalizeProxy 补齐 proxy URL 的 scheme。
func normalizeProxy(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	if strings.HasPrefix(value, "socks5://") {
		return "socks5h://" + strings.TrimPrefix(value, "socks5://")
	}
	if strings.HasPrefix(value, "socks://") {
		return "socks5h://" + strings.TrimPrefix(value, "socks://")
	}
	return value
}
