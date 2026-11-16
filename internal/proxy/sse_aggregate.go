package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
)

// 本文件提供「OpenAI 风格 SSE 流 → 非流式 chat.completion」的聚合能力。
//
// 来源：原在已移除的厂商账号池体系内，其它路径同样需要
// 「上游只给流式、而客户端要非流式」的聚合，故上提到共享契约包。
// 两个厂商的 SSE 格式都是 OpenAI 兼容（data: {...chat.completion.chunk}），
// 因此这份实现可直接复用，无需各自维护。

// AggregateSSEToCompletion 把 CodeBuddy 的 SSE 流聚合成一个 OpenAI 非流式
// chat.completion 响应（移植自 Node 版 aggregateSseToCompletion）。
//
// 覆盖：content 分片拼接、reasoning_content 拼接、tool_calls 按 index 合并
// （name/arguments 各自累加）、usage 取最后一个非空块。
func SSEAggregateToCompletion(sseText []byte) []byte {
	type toolAcc struct {
		ID        string
		Type      string
		Name      string
		Arguments string
	}
	toolCalls := map[int]*toolAcc{}

	var (
		id, model string
		created   int64
		finish    = "stop"
		usage     map[string]any
		content   strings.Builder
		reasoning strings.Builder
	)

	sc := bufio.NewScanner(bytes.NewReader(sseText))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // 跳过畸形分片（与 Node 版一致，不因个别坏块整包失败）
		}
		if s, ok := chunk["id"].(string); ok && s != "" {
			id = s
		}
		if s, ok := chunk["model"].(string); ok && s != "" {
			model = s
		}
		if f, ok := chunk["created"].(float64); ok {
			created = int64(f)
		}
		if u, ok := chunk["usage"].(map[string]any); ok && len(u) > 0 {
			usage = u
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		if choice == nil {
			continue
		}
		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			finish = fr
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		if s, ok := delta["content"].(string); ok {
			content.WriteString(s)
		}
		if s, ok := delta["reasoning_content"].(string); ok {
			reasoning.WriteString(s)
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, raw := range tcs {
				tc, _ := raw.(map[string]any)
				if tc == nil {
					continue
				}
				idx := 0
				if f, ok := tc["index"].(float64); ok {
					idx = int(f)
				}
				acc := toolCalls[idx]
				if acc == nil {
					acc = &toolAcc{Type: "function"}
					toolCalls[idx] = acc
				}
				if s, ok := tc["id"].(string); ok && s != "" {
					acc.ID = s
				}
				if s, ok := tc["type"].(string); ok && s != "" {
					acc.Type = s
				}
				if fn, ok := tc["function"].(map[string]any); ok {
					if s, ok := fn["name"].(string); ok {
						acc.Name += s
					}
					if s, ok := fn["arguments"].(string); ok {
						acc.Arguments += s
					}
				}
			}
		}
	}

	message := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		idx := make([]int, 0, len(toolCalls))
		for k := range toolCalls {
			idx = append(idx, k)
		}
		SortInts(idx)
		list := make([]any, 0, len(idx))
		for _, i := range idx {
			tc := toolCalls[i]
			list = append(list, map[string]any{
				"id": tc.ID, "type": tc.Type,
				"function": map[string]any{"name": tc.Name, "arguments": tc.Arguments},
			})
		}
		message["tool_calls"] = list
	}

	out := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
	}
	if usage != nil {
		out["usage"] = usage
	}

	b, err := json.Marshal(out)
	if err != nil {
		return []byte(`{"error":{"message":"aggregate failed"}}`)
	}
	return b
}

func SortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}
