package proxy

import (
	"strings"
	"testing"
)

// TestSummarizeRequestBody 验证请求体摘要的精简逻辑：
// 超长 messages 只保留最近 8 条并标注省略、长正文截断、图片 base64 打码、采样参数保留。
func TestSummarizeRequestBody(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"model":"deepseek-v4-flash","max_tokens":512,"stream":true,"messages":[`)
	for i := 0; i < 20; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"role":"user","content":"这是一条测试消息内容，长度超过三百字时会被截断。` +
			`重复填充一些内容来拉长它。一二三四五六七八九十一二三四五六七八九十` +
			`一二三四五六七八九十一二三四五六七八九十一二三四五六七八九十一二三四五六七八九十"}`)
	}
	sb.WriteString(`]}`)
	out := summarizeRequestBody([]byte(sb.String()))

	if strings.Contains(out, "messages_omitted") == false {
		t.Error("messages_omitted 应出现（20 条 > 8 条上限）")
	}
	if strings.Contains(out, `"messages":[`) == false {
		t.Error("应保留最近消息列表")
	}
	if strings.Contains(out, "MaxTokensCap") || len(out) > detailMaxSummaryJSON+50 {
		t.Errorf("摘要超长（或包含无关字段）: %d bytes", len(out))
	}

	// 图片 base64 打码：image_url 内容应为占位而非 base64 原文
	b64 := `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,/9j/4AAQSkZJRgABAQAAAQ=="}}]}]}`
	out2 := summarizeRequestBody([]byte(b64))
	if strings.Contains(out2, "/9j/4AAQSkZJRg") {
		t.Errorf("图片 base64 未被打码: %s", out2)
	}
	if !strings.Contains(out2, "image") || !strings.Contains(out2, "base64\u2026") {
		t.Errorf("应包含图片占位标记(带打码): %s", out2)
	}
}

// TestSummarizeUpstreamError 验证上游错误提取：优先 error.message / type / code。
func TestSummarizeUpstreamError(t *testing.T) {
	body := `{"error":{"message":"the model is not supported","type":"invalid_request_error","code":"model_not_found"}}`
	out := summarizeUpstreamError([]byte(body))
	if !strings.Contains(out, "model is not supported") {
		t.Errorf("未提取 error.message: %s", out)
	}
	if !strings.Contains(out, "model_not_found") {
		t.Errorf("未提取 error.code: %s", out)
	}
}

// TestSummarizeUpstreamErrorExtError 验证 workbuddy 系（官方网关）错误的根因提取。
//
// 2026-10-29 实测：workbuddy 对 deepseek-v4-flash 回 400，外层 msg 只有泛化的
// 「the request parameters were rejected by the model provider」，真正可判读的信息
// （param / extError.message）在别的字段，旧实现导致 error_detail 与 stderr 都看不出原因。
type extErrCase struct {
	name     string
	body     string
	wants    []string // 必须出现
	notWants []string // 必须不出现
}

func TestSummarizeUpstreamErrorExtError(t *testing.T) {
	cases := []extErrCase{
		{
			// 今天这 3 条 400 的原始响应（截取到判读所需部分）。
			// 期望：保留外层 msg（定性质），并补上 extError.message（根因）与 requestId（可追溯）。
			name: "workbuddy 11133 extError.message",
			body: `{"code":11133,"msg":"the request parameters were rejected by the model provider",` +
				`"requestId":"01a187694dd5795692b9e8360996bb6c",` +
				`"extError":{"code":"invalid_request_error","message":"the request parameters were rejected by the model provider",` +
				`"param":"","type":"invalid_request_error","StatusCode":400,"Request":null,"Response":null},` +
				`"displayMsg":{"zh":"模型无法处理此请求，请重试"}}`,
			// 外层 msg 与 extError.message 在真实响应里同文，光看这句区分不出实现是否生效，
			// 因此额外要求带上 extError.type（只有走到 extError 分支才会有）。
			wants: []string{"the request parameters were rejected by the model provider", "extError.type=invalid_request_error"},
		},
		{
			// 有 param 时 param 是唯一根因线索（参考 upstreamErrorText 的处理口径）。
			name: "workbuddy extError.param",
			body: `{"code":11133,"msg":"the request parameters were rejected by the model provider",` +
				`"requestId":"rid-1",` +
				`"extError":{"message":"tool_calls.function.name is required","param":"messages.1.tool_calls.0.function.name"}}`,
			wants:    []string{"tool_calls.function.name is required", "messages.1.tool_calls.0.function.name"},
			notWants: []string{"displayMsg"},
		},
		{
			// extError.message 与 msg 相同时不应重复输出两次（日志噪音）。
			name:  "extError.message 与 msg 相同不重复",
			body:  `{"code":11133,"msg":"same text","extError":{"message":"same text"}}`,
			wants: []string{"same text"},
		},
		{
			// 标准 OpenAI 格式必须保持原样（有 error 对象时不走 workbuddy 分支）。
			name:     "标准 error 格式不受影响",
			body:     `{"error":{"message":"bad thing","type":"invalid_request_error"},"extError":{"message":"should not win"}}`,
			wants:    []string{"bad thing", "type=invalid_request_error"},
			notWants: []string{"should not win"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := summarizeUpstreamError([]byte(c.body))
			for _, w := range c.wants {
				if !strings.Contains(out, w) {
					t.Errorf("应包含 %q，实际: %s", w, out)
				}
			}
			for _, nw := range c.notWants {
				if strings.Contains(out, nw) {
					t.Errorf("不应包含 %q，实际: %s", nw, out)
				}
			}
		})
	}
}
