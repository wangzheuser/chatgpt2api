package register

import (
	"crypto/sha3"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"time"
)

// 本文件移植 dev 的 utils/pow.py：Sentinel SDK 结构变更时的 legacy 降级路径。
// 算法与 dev 保持一致（SHA3-512 hashcash + 浏览器指纹配置数组）。

var powCores = []int{8, 16, 24, 32}

var powDocumentKeys = []string{"__reactContainer$fzelfjyxej8", "_reactListening5dehydibo78", "location"}

var powScreenResolutions = [][2]int{{1920, 1080}, {1440, 900}, {2560, 1440}, {3840, 2160}}

var powNavigatorKeys = []string{
	"registerProtocolHandler−function registerProtocolHandler() { [native code] }",
	"storage−[object StorageManager]",
	"locks−[object LockManager]",
	"appCodeName−Mozilla",
	"permissions−[object Permissions]",
	"share−function share() { [native code] }",
	"webdriver−false",
	"managed−[object NavigatorManagedData]",
	"canShare−function canShare() { [native code] }",
	"vendor−Google Inc.",
	"mediaDevices−[object MediaDevices]",
	"vibrate−function vibrate() { [native code] }",
	"storageBuckets−[object StorageBucketManager]",
	"mediaCapabilities−[object MediaCapabilities]",
	"cookieEnabled−true",
	"virtualKeyboard−[object VirtualKeyboard]",
	"product−Gecko",
	"presentation−[object Presentation]",
	"onLine−true",
	"mimeTypes−[object MimeTypeArray]",
	"credentials−[object CredentialsContainer]",
	"serviceWorker−[object ServiceWorkerContainer]",
	"keyboard−[object Keyboard]",
	"gpu−[object GPU]",
	"doNotTrack",
	"serial−[object Serial]",
	"pdfViewerEnabled−true",
	"language−zh-CN",
	"geolocation−[object Geolocation]",
	"userAgentData−[object NavigatorUAData]",
	"getUserMedia−function getUserMedia() { [native code] }",
	"sendBeacon−function sendBeacon() { [native code] }",
	"hardwareConcurrency−32",
	"windowControlsOverlay−[object WindowControlsOverlay]",
}

var powWindowKeys = []string{
	"0", "window", "self", "document", "name", "location", "customElements", "history", "navigation",
	"innerWidth", "innerHeight", "scrollX", "scrollY", "visualViewport", "screenX", "screenY",
	"outerWidth", "outerHeight", "devicePixelRatio", "screen", "chrome", "navigator", "onresize",
	"performance", "crypto", "indexedDB", "sessionStorage", "localStorage", "scheduler", "alert",
	"atob", "btoa", "fetch", "matchMedia", "postMessage", "queueMicrotask", "requestAnimationFrame",
	"setInterval", "setTimeout", "caches", "__NEXT_DATA__", "__BUILD_MANIFEST", "__NEXT_PRELOADREADY",
}

// marshalNoEscape 输出紧凑 JSON 且不转义 HTML 字符（对齐 Python 的 ensure_ascii=False）。
func marshalNoEscape(value any) ([]byte, error) {
	builder := &strings.Builder{}
	encoder := json.NewEncoder(builder)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return []byte(strings.TrimRight(builder.String(), "\n")), nil
}

// legacyParseTime 复刻 dev 的 _legacy_parse_time（固定 GMT-0500 文案）。
func legacyParseTime() string {
	return time.Now().In(time.FixedZone("EST", -5*3600)).Format("Mon Jan 02 2006 15:04:05") +
		" GMT-0500 (Eastern Standard Time)"
}

// buildPowConfig 生成 25 元素指纹配置数组，顺序与 dev 的 build_pow_config 一致。
func buildPowConfig(userAgent, sdkURL string) []any {
	screen := powScreenResolutions[rand.Intn(len(powScreenResolutions))]
	navigator := powNavigatorKeys[rand.Intn(len(powNavigatorKeys))]
	documentKey := powDocumentKeys[rand.Intn(len(powDocumentKeys))]
	windowKey := powWindowKeys[rand.Intn(len(powWindowKeys))]
	core := powCores[rand.Intn(len(powCores))]
	if strings.TrimSpace(sdkURL) == "" {
		sdkURL = defaultPowScript
	}
	perfMs := float64(time.Now().UnixNano()) / 1e6
	return []any{
		screen[0] + screen[1],
		legacyParseTime(),
		4294705152,
		1,
		userAgent,
		sdkURL,
		"",
		"en-US",
		"en-US,es-US,en,es",
		rand.Float64(),
		navigator,
		documentKey,
		windowKey,
		perfMs,
		newDeviceID(),
		"",
		core,
		float64(time.Now().UnixMilli()) - perfMs,
		0, 0, 0, 0, 0, 0,
		0,
	}
}

// buildLegacyRequirementsToken 生成 legacy prepare token（gAAAAAC + base64(config)）。
func buildLegacyRequirementsToken(userAgent, sdkURL string) string {
	payload, err := marshalNoEscape(buildPowConfig(userAgent, sdkURL))
	if err != nil {
		return ""
	}
	return "gAAAAAC" + base64.StdEncoding.EncodeToString(payload)
}

// powGenerate 求解 hashcash：SHA3-512(seed + base64(finalJSON)) 前缀 <= difficulty。
func powGenerate(seed, difficulty string, config []any) (string, bool) {
	target, err := hex.DecodeString(strings.TrimSpace(difficulty))
	if err != nil || len(target) == 0 {
		return "", false
	}
	diffLen := len(target)

	prefix, err := marshalNoEscape(config[:3])
	if err != nil {
		return "", false
	}
	static1 := string(prefix[:len(prefix)-1]) + ","

	middle, err := marshalNoEscape(config[4:9])
	if err != nil {
		return "", false
	}
	static2 := "," + string(middle[1:len(middle)-1]) + ","

	suffix, err := marshalNoEscape(config[10:])
	if err != nil {
		return "", false
	}
	static3 := "," + string(suffix[1:])

	seedBytes := []byte(seed)
	hasher := sha3.New512()
	buffer := make([]byte, 0, len(static1)+len(static2)+len(static3)+24)
	encoded := make([]byte, 256)

	const limit = 500000
	for i := 0; i < limit; i++ {
		buffer = buffer[:0]
		buffer = append(buffer, static1...)
		buffer = append(buffer, fmt.Sprintf("%d", i)...)
		buffer = append(buffer, static2...)
		buffer = append(buffer, fmt.Sprintf("%d", i>>1)...)
		buffer = append(buffer, static3...)

		need := base64.StdEncoding.EncodedLen(len(buffer))
		if cap(encoded) < need {
			encoded = make([]byte, need)
		}
		encoded = encoded[:need]
		base64.StdEncoding.Encode(encoded, buffer)

		hasher.Reset()
		hasher.Write(seedBytes)
		hasher.Write(encoded)
		digest := hasher.Sum(nil)

		matched := true
		for index := 0; index < diffLen; index++ {
			if digest[index] < target[index] {
				break
			}
			if digest[index] > target[index] {
				matched = false
				break
			}
		}
		if matched {
			return string(encoded), true
		}
	}
	return "", false
}

// buildProofToken 生成 legacy proof token（gAAAAAB + base64(答案)）。
func buildProofToken(seed, difficulty, userAgent, sdkURL string) (string, error) {
	answer, solved := powGenerate(seed, difficulty, buildPowConfig(userAgent, sdkURL))
	if !solved {
		return "", fmt.Errorf("failed to solve proof token: difficulty=%s", difficulty)
	}
	return "gAAAAAB" + answer, nil
}
