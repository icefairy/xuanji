package proxy

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// MergeSystemMessages 将请求体 messages 中所有 role=system 的消息合并为一条，
// 放到 messages 第一位。部分上游（某些 vLLM/推理服务/网关）不允许出现
// 多个 system 消息，否则返回 400；合并后按 OpenAI 规范 system 必须位于最前。
//
// 规则：
//   - 无 system 或仅一条 system → 原样返回（changed=false）
//   - 多条 system 的 content（字符串）按 \n\n 拼接；content 为数组（多模态）
//     时提取其中 type=text 的 text 片段拼接，其余类型片段丢弃
//   - 非 system 消息保持原顺序
//   - messages 不存在/非数组 → 原样返回
func MergeSystemMessages(body []byte) ([]byte, bool) {
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return body, false
	}
	arr := msgs.Array()
	if len(arr) < 2 {
		return body, false
	}

	var sysParts []string
	var rest []interface{}
	for _, m := range arr {
		if m.Get("role").String() == "system" {
			if c := m.Get("content"); c.Exists() {
				sysParts = append(sysParts, systemContentText(c))
			}
		} else {
			rest = append(rest, m.Value())
		}
	}
	if len(sysParts) <= 1 {
		// 无 system 或仅一条 system：无需合并
		return body, false
	}

	newMsgs := make([]interface{}, 0, len(arr))
	newMsgs = append(newMsgs, map[string]interface{}{
		"role":    "system",
		"content": strings.Join(sysParts, "\n\n"),
	})
	newMsgs = append(newMsgs, rest...)

	nb, err := sjson.SetBytes(body, "messages", newMsgs)
	if err != nil {
		return body, false
	}
	return nb, true
}

// CleanOrphanToolMessages 移除 messages 中「孤儿 tool 消息」：即 role=tool 但
// 缺少/为空的 tool_call_id 的消息。这类消息没有对应的 assistant tool_calls，
// 属于客户端长会话压缩/干预后残留的无效项。部分上游（sglang 托管的 OpenAI
// 兼容端点，如 agnes）对 tool 消息做严格 schema 校验，收到即 400
// "missing field `tool_call_id`"，故转发前统一清理兜底；宽松上游原样透传时
// 通常不报错，但此类消息对模型也毫无语义价值，清理无副作用。
//
// 规则：
//   - 无孤儿 tool 消息 → 原样返回（changed=false），零开销
//   - 其余消息（含正常的 tool 消息、assistant/user/system）原样保留，顺序不变
//   - messages 不存在/非数组 → 原样返回
func CleanOrphanToolMessages(body []byte) ([]byte, bool) {
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return body, false
	}
	arr := msgs.Array()
	if len(arr) == 0 {
		return body, false
	}

	clean := true
	var keep []interface{}
	for _, m := range arr {
		if m.Get("role").String() == "tool" && m.Get("tool_call_id").String() == "" {
			clean = false // 丢弃孤儿 tool 消息
			continue
		}
		keep = append(keep, m.Value())
	}
	if clean {
		return body, false
	}
	nb, err := sjson.SetBytes(body, "messages", keep)
	if err != nil {
		return body, false
	}
	return nb, true
}

// systemContentText 从 system 消息的 content 中提取纯文本：
// content 为字符串 → 直接返回；为数组（OpenAI 多模态格式）→ 拼接 type=text 的 text；
// 其他类型 → 空串。
func systemContentText(c gjson.Result) string {
	if c.IsObject() || c.IsArray() {
		var parts []string
		for _, item := range c.Array() {
			if item.Get("type").String() == "text" {
				if t := item.Get("text").String(); t != "" {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return c.String()
}
