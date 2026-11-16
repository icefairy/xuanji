package proxy

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 本文件适配 opencode（Zen）渠道：type=opencode 的上游。
//
// 背景（2026-09-28 实测，证据见 HMEM #4437 / #4438）：
// opencode.ai/zen 的免费模型（*-free 与 big-pickle 等）服务端加了「客户端身份」闸门，
// 非 opencode 本体的请求一律 403 FreeTierError（"free tier can only be used from
// within OpenCode"）。闸门要求**三条件同时满足**，缺一即 403：
//
//	① 请求体 stream=true（非流式请求直接拒绝）
//	② 请求体 tools 数组必须同时声明小写 read 与 shell（bash 亦可）
//	   —— 名字大小写敏感：实测 Read/Bash 大写的请求仍 403
//	③ 请求头 User-Agent 形如 opencode/<channel>/<ver>/<client>（<1.17.0 会 426）
//	   且 x-opencode-session 匹配 ^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$
//
// 其余实测结论：
//   - 凭证用字面量 Bearer public 即可（免费层匿名，不消耗用户 key）
//   - x-opencode-client / x-opencode-project 可省；b3 / traceparent 非必需
//   - TLS 指纹无关：curl / Go / curl_cffi(Chrome/Safari/FF) / uTLS 复刻 Bun ClientHello
//     全部 403，而带上 tools 后同一 Go 客户端即 200 —— 闸门纯在 HTTP 层
//
// 因此本渠道只需在转发前把三条件补齐，无需 TLS 伪装，也无需账号池。

const (
	// opencodePublicKey 是免费层匿名凭证（实测 Bearer public 即通过）。
	opencodePublicKey = "public"
	// opencodeUserAgent 对齐真实 opencode CLI 的 UA。
	// 上游对版本有下限校验（<1.17.0 返回 426），升级 opencode 后可同步调整。
	opencodeUserAgent = "opencode/latest/2.0.18/cli"
)

// opencodeGateTools 是闸门要求声明的工具名（小写，服务端大小写敏感）。
// shell 与 bash 二选一即可满足；这里都补齐以免上游只认其中一个（实测两者均可）。
var opencodeGateTools = []string{"read", "shell"}

// opencodeSessionHeaders 是下游客户端可能携带的「会话标识」头，按优先级排列。
// 用于派生稳定的 x-opencode-session：同一会话稳定有助于上游 prompt 缓存命中
// （opencode 官方用该头把同一对话的多轮请求路由到同一 GPU 复用 KV 缓存）。
var opencodeSessionHeaders = []string{
	"x-opencode-session",
	"x-conversation-id",
	"x-session-id",
	"x-session-affinity",
	"session-id",
	"session_id",
}

// isOpencodeUpstream 判断是否走 opencode 渠道（type=opencode）。
func isOpencodeUpstream(up *config.Upstream) bool {
	return up != nil && up.IsOpencode()
}

// 以下 IsOpencode / ApplyOpencodeGate / SetOpencodeHeaders / SetOpencodeAuth /
// OpencodeSessionID 是给「非转发路径」复用的导出入口（目前是管理端
// /admin/upstreams/{name}/test 的上游测试）。闸门三条件是上游强制的，
// 任何直连该上游的代码路径都必须补齐，否则必然 403 FreeTierError。

// IsOpencode 报告上游是否为 opencode 渠道（isOpencodeUpstream 的导出版）。
func IsOpencode(up *config.Upstream) bool { return isOpencodeUpstream(up) }

// ApplyOpencodeGate 见 applyOpencodeGate。
func ApplyOpencodeGate(body []byte, clientStream bool) ([]byte, bool) {
	return applyOpencodeGate(body, clientStream)
}

// SetOpencodeHeaders 见 setOpencodeHeaders。
func SetOpencodeHeaders(req *http.Request, sessionID string) { setOpencodeHeaders(req, sessionID) }

// SetOpencodeAuth 见 setOpencodeAuth。
func SetOpencodeAuth(req *http.Request, up *config.Upstream) { setOpencodeAuth(req, up) }

// OpencodeSessionID 见 opencodeSessionID。
func OpencodeSessionID(r *http.Request) string { return opencodeSessionID(r) }

// applyOpencodeGate 为 opencode 上游补齐免费层闸门的三条件中「请求体」两条：
// 强制 stream=true、补齐 tools 声明。返回新 body 与是否发生变更。
//
// clientStream 是客户端**原始**的流式意图（非上游实际发送值）：客户端要非流式时，
// 上游仍必须收流式（条件①），网关随后把 SSE 聚合回非流式响应。
//
// tools 补齐策略（最小干预，避免破坏客户端的工具调用）：
//   - 客户端没传 tools：注入 read/shell 桩，并设 tool_choice="none" —— 桩仅用于过闸，
//     不允许模型真的去调用（否则客户端会收到它不认识的 tool_calls）。
//   - 客户端自带 tools：只补缺失的 read/shell 桩，保留客户端原有 tool_choice 与工具，
//     不影响真实 agent 的工具调用。
func applyOpencodeGate(body []byte, clientStream bool) ([]byte, bool) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body, false
	}
	changed := false
	nb := body

	if !gjson.GetBytes(nb, "stream").Bool() {
		if v, err := sjson.SetBytes(nb, "stream", true); err == nil {
			nb, changed = v, true
		}
	}

	required := map[string]bool{}
	for _, tl := range gjson.GetBytes(nb, "tools").Array() {
		if n := tl.Get("function.name").String(); n != "" {
			required[n] = true
		}
	}
	var missing []string
	for _, name := range opencodeGateTools {
		if !required[name] {
			missing = append(missing, name)
		}
	}
	// bash 已存在时无需再补 shell（服务端二者等价）。
	if required["bash"] {
		filtered := missing[:0]
		for _, name := range missing {
			if name != "shell" {
				filtered = append(filtered, name)
			}
		}
		missing = filtered
	}
	if len(missing) > 0 {
		hadTools := len(required) > 0
		var tools []any
		for _, tl := range gjson.GetBytes(nb, "tools").Array() {
			tools = append(tools, tl.Value())
		}
		for _, name := range missing {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": name,
					// 桩工具只用于满足上游校验，描述刻意留空/简短，降低被模型选中的概率。
					"description": "internal",
					"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
				},
			})
		}
		if v, err := sjson.SetBytes(nb, "tools", tools); err == nil {
			nb, changed = v, true
		}
		// 客户端原本没有工具：禁止模型调用桩工具（否则回传的 tool_calls 客户端无法处理）。
		// 注意仅在此情形改写 tool_choice，客户端自带工具时保持原值。
		if !hadTools {
			if v, err := sjson.SetBytes(nb, "tool_choice", "none"); err == nil {
				nb = v
			}
		}
	}
	if !clientStream {
		// 客户端要非流式：上游只认流式，仍需 usage 以便计费统计。
		if v, err := sjson.SetBytes(nb, "stream_options.include_usage", true); err == nil {
			nb, changed = v, true
		}
	}
	return nb, changed
}

// setOpencodeHeaders 注入闸门条件③要求的身份头。
func setOpencodeHeaders(req *http.Request, sessionID string) {
	req.Header.Set("User-Agent", opencodeUserAgent)
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-session", sessionID)
	if req.Header.Get("x-opencode-project") == "" {
		req.Header.Set("x-opencode-project", "xuanji")
	}
}

// setOpencodeAuth 设置 opencode 上游的认证头。
// 免费层匿名（Bearer public）；用户填了真实 Zen key 时透传（付费模型用）。
func setOpencodeAuth(req *http.Request, up *config.Upstream) {
	key := strings.TrimSpace(up.APIKey)
	if key == "" {
		key = opencodePublicKey
	}
	req.Header.Set("Authorization", "Bearer "+key)
}

// opencodeSessionID 生成符合上游校验格式（^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$）的会话 ID。
//
// 优先由下游客户端的会话头派生（同一会话稳定 → 上游 prompt 缓存可复用）；
// 客户端未提供时随机生成。注意不能所有请求共用一个固定值：上游会把并行任务
// 视为同一会话，属于高风险特征（社区实现的一致建议）。
func opencodeSessionID(r *http.Request) string {
	seed := ""
	if r != nil {
		for _, h := range opencodeSessionHeaders {
			if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
				seed = h + ":" + v
				break
			}
		}
	}
	var digest [32]byte
	if seed != "" {
		digest = sha256.Sum256([]byte(seed))
	} else {
		if _, err := rand.Read(digest[:]); err != nil {
			// 随机源异常时退化为固定种子（仍满足格式，仅失去会话隔离）。
			digest = sha256.Sum256([]byte("xuanji-opencode"))
		}
	}
	return "ses_" + hex12(digest[:]) + base62N(digest[:], 14)
}

const opencodeBase62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// hex12 取字节序列前 6 字节的小写十六进制（12 字符）。
func hex12(b []byte) string {
	const digits = "0123456789abcdef"
	var sb strings.Builder
	for i := 0; i < 6 && i < len(b); i++ {
		sb.WriteByte(digits[b[i]>>4])
		sb.WriteByte(digits[b[i]&0x0f])
	}
	return sb.String()
}

// base62N 从字节序列派生 n 个 base62 字符（取模映射，格式合规即可）。
func base62N(b []byte, n int) string {
	var sb strings.Builder
	for i := 0; sb.Len() < n; i++ {
		sb.WriteByte(opencodeBase62[int(b[i%len(b)]+byte(i))%len(opencodeBase62)])
	}
	return sb.String()
}

// opencodeModelIsFree 判断模型是否属免费层（用于日志提示与诊断，
// 不参与转发决策——闸门对所有模型统一补齐，付费模型带真实 key 时同样适用）。
func opencodeModelIsFree(model string) bool {
	if strings.EqualFold(model, "big-pickle") {
		return true
	}
	return strings.HasSuffix(strings.ToLower(model), "-free")
}

// describeOpencodeGate 返回便于排查的一行摘要（写入错误详情，便于请求日志定位）。
func describeOpencodeGate(model string, clientStream bool) string {
	free := "paid"
	if opencodeModelIsFree(model) {
		free = "free"
	}
	return fmt.Sprintf("opencode gate: tier=%s client_stream=%v upstream_stream=true", free, clientStream)
}
