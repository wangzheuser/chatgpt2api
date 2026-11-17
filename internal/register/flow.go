package register

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// candidate 是注册成功后待入库的账号信息。
type candidate struct {
	Email        string
	AccessToken  string
	RefreshToken string
	IDToken      string
	SourceType   string
	CreatedAt    string
}

// flow 承载单个注册任务的授权状态机，对应 dev 的 PlatformRegistrar。
type flow struct {
	sess         *session
	runner       *sentinelRunner
	deviceID     string
	codeVerifier string
	authCode     string
	passwordless bool
	mailbox      Mailbox
	proxy        string
	logf         func(string, string)
	codeWaiter   func(context.Context, Mailbox) (string, error)
}

func newFlow(sess *session, runner *sentinelRunner, mailbox Mailbox, proxy string, logf func(string, string)) (*flow, error) {
	verifier, _, err := pkcePairVerifier()
	if err != nil {
		return nil, err
	}
	if logf == nil {
		logf = func(string, string) {}
	}
	return &flow{
		sess:         sess,
		runner:       runner,
		deviceID:     newDeviceID(),
		codeVerifier: verifier,
		mailbox:      mailbox,
		proxy:        proxy,
		logf:         logf,
	}, nil
}

// pkcePairVerifier 返回 verifier 与 challenge。
func pkcePairVerifier() (string, string, error) {
	verifier, challenge := pkcePair()
	if verifier == "" || challenge == "" {
		return "", "", errors.New("pkce_generation_failed")
	}
	return verifier, challenge, nil
}

func (f *flow) jsonHeaders(referer string) map[string]string {
	headers := commonHeaders()
	headers["referer"] = referer
	headers["oai-device-id"] = f.deviceID
	for name, value := range traceHeaders() {
		headers[name] = value
	}
	return headers
}

func (f *flow) navigate(referer string) map[string]string {
	headers := navigateHeaders()
	if referer != "" {
		headers["referer"] = referer
	}
	return headers
}

// withSentinel 生成 flow 对应的 Sentinel 头并写 oai-sc cookie。
func (f *flow) withSentinel(ctx context.Context, headers map[string]string, flowName string, timeoutMS int) (map[string]string, error) {
	artifacts, err := f.runner.build(ctx, f.sess, f.deviceID, flowName, timeoutMS)
	if err != nil {
		return headers, err
	}
	if artifacts.Degraded != "" {
		f.logf("Sentinel SDK 不可用，已降级到 legacy PoW: "+truncate(artifacts.Degraded, 160), "yellow")
	}
	f.sess.applySentinelCookie(artifacts.OaiSC)
	headers["openai-sentinel-token"] = artifacts.Token
	if artifacts.SoToken != "" {
		headers["openai-sentinel-so-token"] = artifacts.SoToken
	} else {
		f.logf("Sentinel 未生成 so-token，create_account 成功率可能偏低", "yellow")
	}
	return headers, nil
}

// request 统一处理 Cloudflare 挑战：命中后刷新 clearance 并重试一次。
func (f *flow) request(ctx context.Context, method, target string, headers map[string]string, body any) (response, error) {
	resp, err := f.sess.do(ctx, method, target, requestOptions{headers: headers, json: body})
	if err != nil {
		return resp, err
	}
	if !isCloudflareChallenge(resp) {
		return resp, nil
	}
	f.logf("检测到 Cloudflare 拦截，尝试刷新 clearance", "yellow")
	if clearanceErr := f.sess.applyClearance(ctx, authBase); clearanceErr != nil {
		return resp, errors.New(cloudflareBlockMessage(resp, "被 Cloudflare 拦截", "clearance 刷新失败: "+clearanceErr.Error()))
	}
	resp, err = f.sess.do(ctx, method, target, requestOptions{headers: headers, json: body})
	if err != nil {
		return resp, err
	}
	if isCloudflareChallenge(resp) {
		return resp, errors.New(cloudflareBlockMessage(resp, "被 Cloudflare 拦截", "Cloudflare clearance 重试仍被拦截"))
	}
	return resp, nil
}

// authorize 执行 platform authorize，对应 dev 的 _platform_authorize。
func (f *flow) authorize(ctx context.Context, email string) error {
	f.logf("开始 platform authorize", "")
	// 设备 cookie 必须落在实际授权域上（生产为 auth.openai.com，测试可注入本地域名）。
	authHost := hostOf(authBase)
	for _, domain := range []string{authHost, "." + authHost, "openai.com", ".openai.com"} {
		f.sess.setCookie("oai-did", f.deviceID, domain)
	}
	_, challenge := pkcePairFromVerifier(f.codeVerifier)
	target := buildAuthorizeURL(email, f.deviceID, challenge)
	headers := f.navigate(platformBase + "/")
	resp, err := f.request(ctx, "GET", target, headers, nil)
	if err != nil {
		return err
	}
	if resp.status != 200 {
		return fmt.Errorf("platform_authorize_http_%d, %s", resp.status, resp.debug(400))
	}
	f.passwordless = strings.Contains(strings.ToLower(resp.url), "/email-verification")
	mode := "challenge"
	if f.passwordless {
		mode = "passwordless"
	}
	f.logf(fmt.Sprintf("platform authorize 完成 mode=%s url=%s", mode, truncate(resp.url, 160)), "")
	return nil
}

// sendOTP 切换到 passwordless 并发送验证码，对应 dev 的 _start_passwordless_signup。
func (f *flow) sendOTP(ctx context.Context) error {
	f.logf("开始切换 passwordless signup 并发送验证码", "")
	target := authBase + "/api/accounts/passwordless/send-otp"
	headers := f.jsonHeaders(authBase + "/create-account/password")
	resp, err := f.request(ctx, "POST", target, headers, nil)
	if err != nil {
		return err
	}
	if resp.status != 200 {
		return fmt.Errorf("passwordless_send_otp_http_%d, %s", resp.status, resp.debug(400))
	}
	f.passwordless = true
	f.logf("passwordless signup 验证码发送完成", "")
	return nil
}

// validateOTP 校验验证码，对应 dev 的 validate_otp。
func (f *flow) validateOTP(ctx context.Context, code string) error {
	f.logf("开始校验验证码 "+code, "")
	target := authBase + "/api/accounts/email-otp/validate"
	headers := f.jsonHeaders(authBase + "/email-verification")
	resp, err := f.request(ctx, "POST", target, headers, map[string]string{"code": code})
	if err != nil {
		return err
	}
	if resp.status != 200 {
		retryHeaders, sentinelErr := f.withSentinel(ctx, f.jsonHeaders(authBase+"/email-verification"), "authorize_continue", defaultSentinelTimeoutMS)
		if sentinelErr == nil {
			resp, err = f.request(ctx, "POST", target, retryHeaders, map[string]string{"code": code})
			if err != nil {
				return err
			}
		}
	}
	if resp.status != 200 {
		return fmt.Errorf("validate_otp_http_%d, %s", resp.status, resp.debug(300))
	}
	data := resp.json()
	if continueURL := extractContinueURL(data); continueURL != "" {
		if continueErr := f.authorizeContinue(ctx, continueURL); continueErr != nil {
			return continueErr
		}
	}
	f.logf("验证码校验完成", "")
	return nil
}

// authorizeContinue 跟随 continue_url，对应 dev 的 _authorize_continue。
func (f *flow) authorizeContinue(ctx context.Context, continueURL string) error {
	target := strings.TrimSpace(continueURL)
	if target == "" {
		return nil
	}
	if !strings.HasPrefix(strings.ToLower(target), "http") {
		target = authBase + "/" + strings.TrimPrefix(target, "/")
	}
	headers := f.navigate(authBase + "/email-verification")
	resp, err := f.request(ctx, "GET", target, headers, nil)
	if err != nil {
		return err
	}
	if resp.status != 200 && resp.status != 302 {
		return fmt.Errorf("authorize_continue_http_%d, %s", resp.status, resp.debug(300))
	}
	f.logf("authorize/continue 完成 url="+truncate(resp.url, 160), "")
	return nil
}

// createAccount 创建账号资料并提取 OAuth code，对应 dev 的 _create_account。
func (f *flow) createAccount(ctx context.Context, name, birthdate string) error {
	f.logf("开始创建账号资料", "")
	target := authBase + "/api/accounts/create_account"
	headers, sentinelErr := f.withSentinel(ctx, f.jsonHeaders(authBase+"/about-you"), "oauth_create_account", defaultSentinelTimeoutMS)
	if sentinelErr != nil {
		f.logf("Sentinel 生成失败，继续尝试创建账号: "+sentinelErr.Error(), "yellow")
	}
	resp, err := f.request(ctx, "POST", target, headers, map[string]string{"name": name, "birthdate": birthdate})
	if err != nil {
		return err
	}
	if resp.status != 200 && resp.status != 302 {
		data := resp.json()
		if message, ok := data["message"].(string); ok && strings.Contains(message, "Failed to create account") {
			f.logf("创建账号失败提示: 邮箱域名很可能因滥用被封禁，请更换邮箱域名", "yellow")
		}
		return fmt.Errorf("create_account_http_%d, %s", resp.status, resp.debug(400))
	}
	data := resp.json()
	continueURL := strings.TrimSpace(fmt.Sprint(data["continue_url"]))
	f.authCode = oauthCallbackCode(continueURL)
	if f.authCode == "" {
		return errors.New("create_account 响应缺少 oauth code")
	}
	f.logf("创建账号资料完成", "")
	return nil
}

// exchangeTokens 用 OAuth code 换 token，对应 dev 的 request_platform_oauth_token。
func (f *flow) exchangeTokens(ctx context.Context) (map[string]string, error) {
	f.logf("开始换 token", "")
	target := authBase + "/api/accounts/oauth/token"
	headers := map[string]string{
		"accept":             "*/*",
		"accept-language":    "zh-CN,zh;q=0.9",
		"auth0-client":       platformAuth0Client,
		"cache-control":      "no-cache",
		"content-type":       "application/json",
		"origin":             platformBase,
		"pragma":             "no-cache",
		"priority":           "u=1, i",
		"referer":            platformBase + "/",
		"sec-ch-ua":          registerSecChUA,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"Windows"`,
		"sec-fetch-dest":     "empty",
		"sec-fetch-mode":     "cors",
		"sec-fetch-site":     "same-site",
		"user-agent":         registerUserAgent,
	}
	payload := map[string]string{
		"client_id":     platformOAuthClientID,
		"code_verifier": f.codeVerifier,
		"grant_type":    "authorization_code",
		"code":          f.authCode,
		"redirect_uri":  platformOAuthRedirect,
	}
	resp, err := f.request(ctx, "POST", target, headers, payload)
	if err != nil {
		return nil, err
	}
	if resp.status != 200 {
		return nil, fmt.Errorf("oauth_token_http_%d, %s", resp.status, resp.debug(300))
	}
	data := resp.json()
	result := map[string]string{
		"access_token":  strings.TrimSpace(fmt.Sprint(data["access_token"])),
		"refresh_token": strings.TrimSpace(fmt.Sprint(data["refresh_token"])),
		"id_token":      strings.TrimSpace(fmt.Sprint(data["id_token"])),
	}
	if result["access_token"] == "" {
		return nil, errors.New("token 换取失败")
	}
	f.logf("token 换取完成", "")
	return result, nil
}

// run 执行完整注册流程，返回待入库账号。
func (f *flow) run(ctx context.Context) (candidate, error) {
	email := f.mailbox.Address
	if email == "" {
		return candidate{}, errors.New("邮箱服务未返回 address")
	}
	f.logf("邮箱创建完成: "+email, "")
	if err := f.authorize(ctx, email); err != nil {
		return candidate{}, err
	}
	if !f.passwordless {
		if err := f.sendOTP(ctx); err != nil {
			return candidate{}, err
		}
	}
	code, err := f.waitCode(ctx)
	if err != nil {
		return candidate{}, err
	}
	if err := f.validateOTP(ctx, code); err != nil {
		return candidate{}, err
	}
	firstName, lastName := randomName()
	if err := f.createAccount(ctx, firstName+" "+lastName, randomBirthdate()); err != nil {
		return candidate{}, err
	}
	tokens, err := f.exchangeTokens(ctx)
	if err != nil {
		return candidate{}, err
	}
	return candidate{
		Email:        email,
		AccessToken:  tokens["access_token"],
		RefreshToken: tokens["refresh_token"],
		IDToken:      tokens["id_token"],
		SourceType:   "web",
		CreatedAt:    nowISO(),
	}, nil
}

// waitCode 由 service 注入的邮箱轮询实现。
func (f *flow) waitCode(ctx context.Context) (string, error) {
	if f.codeWaiter == nil {
		return "", errors.New("code waiter missing")
	}
	return f.codeWaiter(ctx, f.mailbox)
}

// pkcePairFromVerifier 由 verifier 还原 challenge。
func pkcePairFromVerifier(verifier string) (string, string) {
	_, challenge := pkcePairFromString(verifier)
	return verifier, challenge
}

// marshalTruncated 用于错误上下文中的 JSON 片段。
func marshalTruncated(value any, limit int) string {
	payload, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return truncate(string(payload), limit)
}

// absoluteURL 把相对 continue_url 补全为绝对地址。
func absoluteURL(base, target string) string {
	if strings.HasPrefix(strings.ToLower(target), "http") {
		return target
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return target
	}
	joined, err := parsed.Parse(strings.TrimPrefix(target, "/"))
	if err != nil {
		return target
	}
	return joined.String()
}
