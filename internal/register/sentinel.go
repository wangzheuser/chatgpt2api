package register

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// sentinelRunnerScript 是 dev 分支的 Node SDK runner，原样内嵌以保持行为一致。
//
//go:embed sentinel_sdk_runner.js
var sentinelRunnerScript []byte

// sentinelArtifacts 对应 dev 的 SentinelArtifacts。
type sentinelArtifacts struct {
	Token             string
	OaiSC             string
	SoToken           string
	SDKURL            string
	SDKVersion        string
	Flow              string
	ObserverTimeoutMS int
	// Degraded 非空表示走了 legacy 降级路径（SDK 结构变更时的兜底）。
	Degraded string
}

// sentinelRunner 负责 SDK 资产缓存与 Node 子进程调用。
type sentinelRunner struct {
	nodeBin string
	mu      sync.Mutex
	script  string
	cache   sdkAssets
}

type sdkAssets struct {
	url     string
	source  string
	fetched time.Time
}

var sdkURLPattern = regexp.MustCompile(`src=['"]([^'"]+/sentinel/[^'"]+/sdk\.js[^'"]*)['"]`)
var sdkVersionPattern = regexp.MustCompile(`/sentinel/([^/]+)/sdk\.js`)

func newSentinelRunner(nodeBin string) *sentinelRunner {
	if strings.TrimSpace(nodeBin) == "" {
		nodeBin = "node"
	}
	return &sentinelRunner{nodeBin: nodeBin}
}

// scriptPath 把内嵌脚本落到临时文件，进程内只写一次。
func (r *sentinelRunner) scriptPath() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.script != "" {
		return r.script, nil
	}
	sum := sha256.Sum256(sentinelRunnerScript)
	name := "chatgpt2api-sentinel-" + hex.EncodeToString(sum[:6]) + ".js"
	target := filepath.Join(os.TempDir(), name)
	if _, err := os.Stat(target); err != nil {
		if writeErr := os.WriteFile(target, sentinelRunnerScript, 0o600); writeErr != nil {
			return "", writeErr
		}
	}
	r.script = target
	return target, nil
}

// run 执行 runner：stdin 传 JSON，stdout 收 JSON。
func (r *sentinelRunner) run(ctx context.Context, payload map[string]any) (map[string]any, error) {
	scriptPath, err := r.scriptPath()
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, r.nodeBin, scriptPath)
	cmd.Stdin = bytes.NewReader(body)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if detail == "" {
			detail = err.Error()
		}
		return nil, fmt.Errorf("sentinel_sdk_runner_failed: %s", truncate(detail, 500))
	}
	output := strings.TrimSpace(stdout.String())
	if output == "" {
		return map[string]any{}, nil
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(output), &parsed); err != nil {
		return nil, fmt.Errorf("sentinel_sdk_runner_output_invalid: %s", truncate(output, 300))
	}
	return parsed, nil
}

// assets 拉取并缓存官方 SDK 源码，对应 dev 的 _load_sentinel_sdk_assets。
func (r *sentinelRunner) assets(ctx context.Context, sess *session) (string, string, error) {
	r.mu.Lock()
	cached := r.cache
	r.mu.Unlock()
	if cached.url != "" && cached.source != "" && time.Since(cached.fetched) < sentinelCacheTTL {
		return cached.url, cached.source, nil
	}
	frameHeaders := map[string]string{
		"User-Agent":         registerUserAgent,
		"sec-ch-ua":          registerSecChUA,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"Windows"`,
		"Accept":             "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Referer":            "https://sentinel.openai.com/",
	}
	frameResp, err := sess.do(ctx, "GET", sentinelFrameURL, requestOptions{headers: frameHeaders})
	if err != nil {
		return "", "", err
	}
	match := sdkURLPattern.FindStringSubmatch(frameResp.text())
	sdkURL := ""
	if match != nil {
		sdkURL = strings.TrimSpace(match[1])
	}
	if frameResp.status != 200 || sdkURL == "" {
		if cached.url != "" && cached.source != "" {
			return cached.url, cached.source, nil
		}
		return "", "", fmt.Errorf("sentinel_frame_http_%d", frameResp.status)
	}
	sourceResp, err := sess.do(ctx, "GET", sdkURL, requestOptions{headers: map[string]string{
		"User-Agent":         registerUserAgent,
		"sec-ch-ua":          registerSecChUA,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"Windows"`,
		"Referer":            sentinelFrameURL,
		"Origin":             "https://sentinel.openai.com",
	}})
	if err != nil {
		return "", "", err
	}
	source := sourceResp.text()
	if sourceResp.status != 200 || !strings.Contains(source, "SentinelSDK") {
		if cached.url != "" && cached.source != "" {
			return cached.url, cached.source, nil
		}
		return "", "", fmt.Errorf("sentinel_sdk_http_%d", sourceResp.status)
	}
	r.mu.Lock()
	r.cache = sdkAssets{url: sdkURL, source: source, fetched: time.Now()}
	r.mu.Unlock()
	return sdkURL, source, nil
}

func sdkVersionOf(sdkURL string) string {
	if match := sdkVersionPattern.FindStringSubmatch(sdkURL); match != nil {
		return match[1]
	}
	trimmed := strings.TrimRight(sdkURL, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) == 0 {
		return ""
	}
	return truncate(parts[len(parts)-1], 64)
}

// buildSDK 走官方 SDK patch 路径生成 Sentinel 产物。
func (r *sentinelRunner) buildSDK(ctx context.Context, sess *session, deviceID, flow string, timeoutMS int) (sentinelArtifacts, error) {
	if timeoutMS <= 0 {
		timeoutMS = defaultSentinelTimeoutMS
	}
	sdkURL, sdkSource, err := r.assets(ctx, sess)
	if err != nil {
		return sentinelArtifacts{}, err
	}
	prepare, err := r.run(ctx, map[string]any{
		"mode":       "prepare",
		"sdk_source": sdkSource,
		"sdk_url":    sdkURL,
		"device_id":  deviceID,
		"flow":       flow,
		"user_agent": registerUserAgent,
	})
	if err != nil {
		return sentinelArtifacts{}, err
	}
	prepareToken := strings.TrimSpace(fmt.Sprint(prepare["prepare_token"]))
	if prepareToken == "" {
		return sentinelArtifacts{}, errors.New("missing_prepare_token")
	}
	reqBody, err := json.Marshal(map[string]string{"p": prepareToken, "id": deviceID, "flow": flow})
	if err != nil {
		return sentinelArtifacts{}, err
	}
	reqResp, err := sess.do(ctx, "POST", sentinelRequestURL, requestOptions{
		headers: map[string]string{
			"Content-Type":       "text/plain;charset=UTF-8",
			"Referer":            sentinelFrameURL,
			"Origin":             "https://sentinel.openai.com",
			"User-Agent":         registerUserAgent,
			"sec-ch-ua":          registerSecChUA,
			"sec-ch-ua-mobile":   "?0",
			"sec-ch-ua-platform": `"Windows"`,
		},
		raw: reqBody,
	})
	if err != nil {
		return sentinelArtifacts{}, err
	}
	reqData := reqResp.json()
	challengeToken := strings.TrimSpace(fmt.Sprint(reqData["token"]))
	if reqResp.status != 200 || challengeToken == "" {
		return sentinelArtifacts{}, fmt.Errorf("sentinel_requirements_http_%d", reqResp.status)
	}
	artifacts, err := r.run(ctx, map[string]any{
		"mode":                "artifacts",
		"sdk_source":          sdkSource,
		"sdk_url":             sdkURL,
		"device_id":           deviceID,
		"flow":                flow,
		"user_agent":          registerUserAgent,
		"prepare_token":       prepareToken,
		"req_data":            reqData,
		"observer_timeout_ms": timeoutMS,
	})
	if err != nil {
		return sentinelArtifacts{}, err
	}
	token := strings.TrimSpace(fmt.Sprint(artifacts["token"]))
	soToken := strings.TrimSpace(fmt.Sprint(artifacts["so_token"]))
	if token == "" {
		return sentinelArtifacts{}, errors.New("sentinel_token_empty")
	}
	return sentinelArtifacts{
		Token:             token,
		OaiSC:             "0" + challengeToken,
		SoToken:           soToken,
		SDKURL:            sdkURL,
		SDKVersion:        sdkVersionOf(sdkURL),
		Flow:              flow,
		ObserverTimeoutMS: timeoutMS,
	}, nil
}

// sentinelHeaders 返回 sentinel/req 与 collector 共用的一组头。
func sentinelHeaders() map[string]string {
	return map[string]string{
		"Content-Type":       "text/plain;charset=UTF-8",
		"Referer":            sentinelFrameURL,
		"Origin":             "https://sentinel.openai.com",
		"User-Agent":         registerUserAgent,
		"sec-ch-ua":          registerSecChUA,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"Windows"`,
	}
}

// build 优先用 SDK 路径，失败后自动降级 legacy PoW（对应 dev 的 except 分支）。
func (r *sentinelRunner) build(ctx context.Context, sess *session, deviceID, flow string, timeoutMS int) (sentinelArtifacts, error) {
	artifacts, err := r.buildSDK(ctx, sess, deviceID, flow, timeoutMS)
	if err == nil {
		return artifacts, nil
	}
	legacy, legacyErr := r.buildLegacy(ctx, sess, deviceID, flow, timeoutMS)
	if legacyErr != nil {
		return sentinelArtifacts{}, fmt.Errorf("sentinel_sdk_failed: %v; sentinel_legacy_failed: %v", err, legacyErr)
	}
	legacy.Degraded = err.Error()
	return legacy, nil
}

// buildLegacy 用本地 PoW 生成 token，不依赖 SDK 源码结构。
func (r *sentinelRunner) buildLegacy(ctx context.Context, sess *session, deviceID, flow string, timeoutMS int) (sentinelArtifacts, error) {
	if timeoutMS <= 0 {
		timeoutMS = defaultSentinelTimeoutMS
	}
	prepare := buildLegacyRequirementsToken(registerUserAgent, defaultPowScript)
	if prepare == "" {
		return sentinelArtifacts{}, errors.New("legacy_prepare_token_failed")
	}
	reqBody, err := json.Marshal(map[string]string{"p": prepare, "id": deviceID, "flow": flow})
	if err != nil {
		return sentinelArtifacts{}, err
	}
	resp, err := sess.do(ctx, "POST", sentinelRequestURL, requestOptions{headers: sentinelHeaders(), raw: reqBody})
	if err != nil {
		return sentinelArtifacts{}, err
	}
	data := resp.json()
	challenge := strings.TrimSpace(fmt.Sprint(data["token"]))
	requirements := data
	if nested, ok := data["requirements"].(map[string]any); ok && len(nested) > 0 {
		requirements = nested
	}
	solved := ""
	if spec := powSpec(requirements); spec != nil {
		seed := strings.TrimSpace(fmt.Sprint(spec["seed"]))
		difficulty := strings.TrimSpace(fmt.Sprint(spec["difficulty"]))
		if seed != "" && difficulty != "" {
			if token, solveErr := buildProofToken(seed, difficulty, registerUserAgent, defaultPowScript); solveErr == nil {
				solved = token
			}
		}
	}
	if solved == "" {
		solved = prepare
	}
	value, err := json.Marshal(map[string]string{
		"p": solved, "t": "", "c": challenge, "id": deviceID, "flow": flow,
	})
	if err != nil {
		return sentinelArtifacts{}, err
	}
	soToken := strings.TrimSpace(fmt.Sprint(data["so_token"]))
	if soToken == "" {
		soToken = strings.TrimSpace(fmt.Sprint(requirements["so_token"]))
	}
	oaiSC := ""
	if challenge != "" {
		oaiSC = "0" + challenge
	}
	return sentinelArtifacts{
		Token:             string(value),
		OaiSC:             oaiSC,
		SoToken:           soToken,
		SDKURL:            defaultPowScript,
		SDKVersion:        "legacy",
		Flow:              flow,
		ObserverTimeoutMS: timeoutMS,
	}, nil
}

// powSpec 从 requirements 里捞出 proofofwork / proof_of_work / pow 节点。
func powSpec(requirements map[string]any) map[string]any {
	for _, key := range []string{"proofofwork", "proof_of_work", "pow"} {
		if node, ok := requirements[key].(map[string]any); ok && len(node) > 0 {
			return node
		}
	}
	return nil
}
