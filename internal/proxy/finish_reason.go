package proxy

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// normalizeFinishReason 把上游的 finish_reason 归一化成 OpenAI 标准取值。
//
// 为什么需要（WorkBuddy / CodeBuddy 官方网关）：
// 官方 SSE 的 choices[0].finish_reason 会出现 `end_turn`——这是从内部 Anthropic
// 表示层漏出来的语义值（app.asar 的 StreamEventConverter 用 Anthropic 事件名），
// 而 OpenAI 协议里只有 stop / length / tool_calls / content_filter。
// 客户端（pi、OpenAI SDK、各家 agent）按枚举判断时，`end_turn` 既不是 stop 也不是
// tool_calls，会被当成异常结束，可能导致重试或多轮循环。
//
// 只做一张最小映射表，不猜别的东西：
//
//	end_turn → stop
//	tool_use → tool_calls（同一 Anthropic 泄漏来源，实测少见但同族）
//
// 其它值原样返回，避免影响非官方上游与既有统计口径。
func normalizeFinishReason(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "end_turn":
		return "stop"
	case "tool_use":
		return "tool_calls"
	default:
		return v
	}
}

// finishReasonNeedsRewrite 快速判定：字符串里是否真的出现了非标准 finish_reason。
//
// 作为逐行透传的性能闸门——绝大多数 SSE 行不含 finish_reason，直接命中 false，
// 不做任何解析。只有命中时才付出 JSON 解析代价（一条流通常只有 1~2 行）。
func finishReasonNeedsRewrite(line []byte) bool {
	if !bytes.Contains(line, []byte(`"finish_reason"`)) {
		return false
	}
	return bytes.Contains(line, []byte(`"end_turn"`)) || bytes.Contains(line, []byte(`"tool_use"`))
}

// normalizeFinishReasonObject 处理整包 JSON 响应体（非流式聚合结果）。
// 无变化时返回原 body（changed=false），避免无谓的内存复制。
func normalizeFinishReasonObject(body []byte) ([]byte, bool) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body, false
	}
	choices := gjson.GetBytes(body, "choices")
	if !choices.IsArray() {
		return body, false
	}
	out := body
	changed := false
	for i, ch := range choices.Array() {
		fr := ch.Get("finish_reason")
		if fr.Type != gjson.String {
			continue
		}
		nv := normalizeFinishReason(fr.String())
		if nv == fr.String() {
			continue
		}
		nb, err := sjson.SetBytes(out, "choices."+strconv.Itoa(i)+".finish_reason", nv)
		if err != nil {
			continue
		}
		out, changed = nb, true
	}
	return out, changed
}

// normalizeFinishReasonSSELine 处理 SSE 单行（保留 `data: ` 前缀）。
//
// 只对确认含非标准值的行做重写，其余行原样返回（逐行透传的性能前提）。
func normalizeFinishReasonSSELine(line []byte) []byte {
	if !finishReasonNeedsRewrite(line) {
		return line
	}
	idx := bytes.Index(line, []byte("data:"))
	if idx < 0 {
		return line
	}
	head := line[:idx]
	payload := bytes.TrimSpace(line[idx+len("data:"):])
	if len(payload) == 0 || payload[0] != '{' {
		return line
	}
	nb, changed := normalizeFinishReasonObject(payload)
	if !changed {
		return line
	}
	out := make([]byte, 0, len(head)+len("data: ")+len(nb))
	out = append(out, head...)
	out = append(out, []byte("data: ")...)
	out = append(out, nb...)
	return out
}
