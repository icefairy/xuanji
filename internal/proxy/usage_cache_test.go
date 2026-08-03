package proxy

import "testing"

// 商汤等上游返回 OpenAI 标准字段 prompt_tokens_details.cached_tokens，
// 没有 DeepSeek 系的 prompt_cache_hit_tokens。验证兜底解析。
func TestParseUsage_OpenAIStandardCachedTokens(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":80}}}`)
	var p, c, hit, miss int64
	if !parseUsage(body, &p, &c, &hit, &miss) {
		t.Fatal("parseUsage 应返回 true")
	}
	if p != 100 || c != 20 {
		t.Fatalf("prompt/completion 解析错误: p=%d c=%d", p, c)
	}
	if hit != 80 {
		t.Fatalf("cached_tokens 兜底失败: hit=%d (期望 80)", hit)
	}
}

// DeepSeek 系字段仍优先。
func TestParseUsage_DeepSeekFieldsPriority(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_cache_hit_tokens":60,"prompt_cache_miss_tokens":40,"prompt_tokens_details":{"cached_tokens":0}}}`)
	var p, c, hit, miss int64
	if !parseUsage(body, &p, &c, &hit, &miss) {
		t.Fatal("parseUsage 应返回 true")
	}
	if hit != 60 || miss != 40 {
		t.Fatalf("DeepSeek 字段优先失败: hit=%d miss=%d (期望 60/40)", hit, miss)
	}
}
