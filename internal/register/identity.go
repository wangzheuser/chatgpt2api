package register

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"time"
)

const (
	defaultSentinelTimeoutMS = 5000
	sentinelCacheTTL         = 30 * time.Minute
)

// 端点地址声明为变量，便于集成测试替换为本地 httptest 服务。
var (
	authBase     = "https://auth.openai.com"
	platformBase = "https://platform.openai.com"

	sentinelFrameURL   = "https://sentinel.openai.com/backend-api/sentinel/frame.html"
	sentinelRequestURL = "https://sentinel.openai.com/backend-api/sentinel/req"
	defaultPowScript   = "https://chatgpt.com/backend-api/sentinel/sdk.js"
)

// platform oauth 常量由 applyDevSecrets 在构建时注入（不写死在源码里）。
var (
	platformOAuthClientID = "app_2SKx67EdpoN0G6j64rFvigXD"
	platformOAuthRedirect = platformBase + "/auth/callback"
	platformOAuthAudience = "https://api.openai.com/v1"
	platformAuth0Client   = "eyJuYW1lIjoiYXV0aDAtc3BhLWpzIiwidmVyc2lvbiI6IjEuMjEuMCJ9"
	registerUserAgent     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"
	registerSecChUA       = `"Chromium";v="145", "Google Chrome";v="145", "Not/A)Brand";v="99"`
	registerSecChUAFull   = `"Chromium";v="145", "Not:A-Brand";v="8", "Google Chrome";v="145"`
)

// randInt 返回 [min,max] 区间的随机整数。
func randInt(minValue, maxValue int) int {
	if maxValue <= minValue {
		return minValue
	}
	span := big.NewInt(int64(maxValue - minValue + 1))
	value, err := rand.Int(rand.Reader, span)
	if err != nil {
		return minValue
	}
	return minValue + int(value.Int64())
}

// randomString 生成指定字符集与长度的随机串。
func randomString(charset string, length int) string {
	buf := make([]byte, length)
	max := big.NewInt(int64(len(charset)))
	for i := range buf {
		value, err := rand.Int(rand.Reader, max)
		if err != nil {
			buf[i] = charset[0]
			continue
		}
		buf[i] = charset[value.Int64()]
	}
	return string(buf)
}

func randomHex(length int) string {
	return randomString("0123456789abcdef", length)
}

// randomName 与 dev 的 _random_name 保持同一候选池。
func randomName() (string, string) {
	first := []string{"James", "Robert", "John", "Michael", "David", "Mary", "Emma", "Olivia"}
	last := []string{"Smith", "Johnson", "Williams", "Brown", "Jones", "Garcia", "Miller"}
	return first[randInt(0, len(first)-1)], last[randInt(0, len(last)-1)]
}

// randomBirthdate 生成 1996-2006 年间的生日，格式 YYYY-MM-DD。
func randomBirthdate() string {
	return fmt.Sprintf("%04d-%02d-%02d", randInt(1996, 2006), randInt(1, 12), randInt(1, 28))
}

// pkcePair 生成 code_verifier 与 S256 challenge。
func pkcePair() (string, string) {
	verifier := base64.RawURLEncoding.EncodeToString([]byte(randomHex(32)))
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge
}

// pkcePairFromString 由 verifier 反算 S256 challenge。
func pkcePairFromString(verifier string) (string, string) {
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// nowISO 返回 UTC RFC3339 时间戳。
func nowISO() string { return time.Now().UTC().Format(time.RFC3339) }

// newDeviceID 生成 UUID v4 形式的设备号。
func newDeviceID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return randomHex(32)
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

// tokenURLSafe 生成 OAuth state / nonce。
func tokenURLSafe(bytes int) string {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		return randomHex(bytes * 2)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// traceHeaders 对应 dev 的 _make_trace_headers。
func traceHeaders() map[string]string {
	traceID := fmt.Sprintf("%d", randInt(1, 1<<62))
	parentID := fmt.Sprintf("%d", randInt(1, 1<<62))
	return map[string]string{
		"traceparent":                 "00-" + randomHex(32) + "-" + fmt.Sprintf("%016x", randInt(1, 1<<62)) + "-01",
		"tracestate":                  "dd=s:1;o:rum",
		"x-datadog-origin":            "rum",
		"x-datadog-parent-id":         parentID,
		"x-datadog-sampling-priority": "1",
		"x-datadog-trace-id":          traceID,
	}
}

// commonHeaders 是 auth 域 JSON 请求的默认头。
func commonHeaders() map[string]string {
	return map[string]string{
		"accept":                      "application/json",
		"accept-language":             "en-US,en;q=0.9",
		"cache-control":               "no-cache",
		"connection":                  "keep-alive",
		"content-type":                "application/json",
		"dnt":                         "1",
		"origin":                      authBase,
		"priority":                    "u=1, i",
		"sec-gpc":                     "1",
		"sec-ch-ua":                   registerSecChUA,
		"sec-ch-ua-arch":              `"x86_64"`,
		"sec-ch-ua-bitness":           `"64"`,
		"sec-ch-ua-full-version-list": registerSecChUAFull,
		"sec-ch-ua-mobile":            "?0",
		"sec-ch-ua-model":             `""`,
		"sec-ch-ua-platform":          `"Windows"`,
		"sec-ch-ua-platform-version":  `"10.0.0"`,
		"sec-fetch-dest":              "empty",
		"sec-fetch-mode":              "cors",
		"sec-fetch-site":              "same-origin",
		"user-agent":                  registerUserAgent,
	}
}

// navigateHeaders 是导航类请求的默认头。
func navigateHeaders() map[string]string {
	headers := commonHeaders()
	headers["accept"] = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"
	headers["sec-fetch-dest"] = "document"
	headers["sec-fetch-mode"] = "navigate"
	headers["sec-fetch-user"] = "?1"
	headers["upgrade-insecure-requests"] = "1"
	delete(headers, "content-type")
	delete(headers, "origin")
	delete(headers, "priority")
	return headers
}

// buildAuthorizeURL 组装 platform authorize 链接（对应 dev 的 _platform_authorize 参数）。
func buildAuthorizeURL(email, deviceID, codeChallenge string) string {
	params := url.Values{}
	params.Set("issuer", authBase)
	params.Set("client_id", platformOAuthClientID)
	params.Set("audience", platformOAuthAudience)
	params.Set("redirect_uri", platformOAuthRedirect)
	params.Set("device_id", deviceID)
	params.Set("screen_hint", "login_or_signup")
	params.Set("max_age", "0")
	params.Set("login_hint", email)
	params.Set("scope", "openid profile email offline_access")
	params.Set("response_type", "code")
	params.Set("response_mode", "query")
	params.Set("state", tokenURLSafe(32))
	params.Set("nonce", tokenURLSafe(32))
	params.Set("code_challenge", codeChallenge)
	params.Set("code_challenge_method", "S256")
	params.Set("auth0Client", platformAuth0Client)
	return authBase + "/api/accounts/authorize?" + params.Encode()
}

// extractContinueURL 对应 dev 的 extract_continue_url。
func extractContinueURL(data map[string]any) string {
	if data == nil {
		return ""
	}
	for _, key := range []string{"continue_url", "continueUrl"} {
		if value, ok := data[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	if page, ok := data["page"].(map[string]any); ok {
		if payload, ok := page["payload"].(map[string]any); ok {
			for _, key := range []string{"continue_url", "continueUrl", "next_url", "nextUrl"} {
				if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
					return strings.TrimSpace(value)
				}
			}
		}
	}
	if sessionInfo, ok := data["oai-client-auth-session"].(map[string]any); ok {
		for _, key := range []string{"continue_url", "continueUrl"} {
			if value, ok := sessionInfo[key].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

// oauthCallbackCode 从回调 URL 中提取 code。
func oauthCallbackCode(raw string) string {
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(parsed.Query().Get("code"))
}
