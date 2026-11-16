package proxy

import (
	"fmt"
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

// validateToolCallNames 校验 messages 中每个 assistant.tool_calls[].function.name
// 是否非空。返回空串表示通过；否则返回面向客户端的可读错误（含精确定位）。
//
// 为什么要主动拦：tool_calls 里的空函数名在某些上游能直接读出原因
// （stepfun: "tool_calls.function.name is required"、agnes: "missing field `name`"），
// 而部分官方网关会把根因吞掉，只回一句泛化的
// code=11133「the request parameters were rejected by the model provider」，
// 且 extError.param 为空、Request/Response 全为 null —— 客户端完全无从下手。
//
// 2026-10-29 实测（deepseek-v4.1-flash，6 次 11133）：本地用同样的
// 空名 tool_call 复现，得到与线上完全一致的 11133，确认为请求本身非法而非上游故障。
// 该请求发往上游必然失败（换号、换上游都一样），故在网关侧提前拒绝：
// 省一次往返、不污染上游健康计数、并给出精确到下标的自查线索。
//
// 不修改 body（无法猜测客户端想调哪个函数），只做校验。
// messages 缺失/非数组、非 JSON、空 body 一律视为通过（不因坏输入篡改流程）。
func validateToolCallNames(body []byte) string {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return ""
	}
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return ""
	}
	for mi, m := range msgs.Array() {
		calls := m.Get("tool_calls")
		if !calls.IsArray() {
			continue
		}
		for ci, tc := range calls.Array() {
			if strings.TrimSpace(tc.Get("function.name").String()) != "" {
				continue
			}
			return fmt.Sprintf(
				"messages[%d].tool_calls[%d].function.name 为空：assistant 的工具调用必须带非空函数名，"+
					"请检查客户端会话历史（通常是上一轮工具调用被截断或序列化时丢了 name），新建任务后重试",
				mi, ci)
		}
	}
	return ""
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
