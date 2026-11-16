package admin

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
)

// tokenACR 从 JWT access_token 的 payload 段读取 acr claim。
//
// 为什么需要它（2026-10-19 实测）：
// WorkBuddy 国际版账号被上游限制对话权限时，对话接口返回
// {"code":11128,"msg":"request illegal"}，但：
//   - /v2/plugin/login/account 仍返回 200（账号信息正常）
//   - refresh_token 仍能换出新的 access_token
//   - 所有模型一律 11128，与请求体内容无关
//
// 从外部看该账号「一切正常」，极难定位。对比两个账号的 token payload 后发现：
// 正常账号 acr=0，被拦截账号 acr=1（Keycloak 的认证上下文等级）。
// 因此把 acr 暴露到管理页，导入时即可发现可疑账号。
//
// ⚠️ 注意 acr 在真实 token 里是**字符串**（"0"/"1"）而非数字，虽然 Keycloak
// 文档与部分实现用数字。两种都兼容，否则会静默解析失败（表现为 known=false）。
//
// 只读 payload，不验签（本地仅用于展示诊断信息，不用于任何鉴权判断）。
// 解析失败一律返回 ok=false，由调用方决定如何展示。
func tokenACR(token string) (int, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, false
	}
	var claims struct {
		ACR json.RawMessage `json:"acr"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || len(claims.ACR) == 0 {
		return 0, false
	}
	// 兼容两种上游编码：数字 1 / 字符串 "1"
	var n int
	if err := json.Unmarshal(claims.ACR, &n); err == nil {
		return n, true
	}
	var s string
	if err := json.Unmarshal(claims.ACR, &s); err == nil {
		if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			return v, true
		}
	}
	return 0, false
}
