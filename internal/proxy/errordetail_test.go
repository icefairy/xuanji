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
