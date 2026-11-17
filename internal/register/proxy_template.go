package register

import "strings"

// proxyUUIDPlaceholder 是代理 URL 中的会话占位符。
//
// 某些轮换型出口代理把会话 ID 放在用户名里：
//
//	http://user.{uuid}:password@gw.example.com:9000
//
// 每次任务建连时把 {uuid} 替换成新的会话 ID，代理服务商就会为本次会话分配独立出口 IP，
// 达到「一次注册换一个 IP」的效果（与 dev 的 utils/proxy_template.py 行为一致）。
const proxyUUIDPlaceholder = "{uuid}"

// renderProxyTemplate 渲染代理 URL 中的 {uuid} 占位符。
//
// - 不含占位符时原样返回（普通代理不受影响）；
// - 同一 URL 内多处 {uuid} 替换成同一个值，保证同一次会话的代理身份一致；
// - 会话 ID 使用 32 位小写十六进制（等同 uuid4().hex，无短横线），兼容不接受短横线的代理商账号规则。
func renderProxyTemplate(proxy string) string {
	text := strings.TrimSpace(proxy)
	if text == "" || !strings.Contains(text, proxyUUIDPlaceholder) {
		return text
	}
	sessionID := randomHex(32)
	return strings.ReplaceAll(text, proxyUUIDPlaceholder, sessionID)
}

// hasProxyTemplate 判断代理是否包含会话占位符。
func hasProxyTemplate(proxy string) bool {
	return strings.Contains(proxy, proxyUUIDPlaceholder)
}
