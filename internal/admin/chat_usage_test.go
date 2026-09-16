package admin

import "testing"

// TestExtractChatUsage_Thinking 覆盖对话调试页的 token 提取。
// 背景：对话调试页展示的是上游**原始** usage（completion_tokens 含思考，
// 不归一化），因此必须同时给出「思考」才能与请求日志（思考单列）对齐：
//
//	请求日志「输出t + 思考t」== 对话调试页「输出」
//
// 思考字段优先 DeepSeek 顶层 thinking_tokens，兜底 OpenAI 标准
// completion_tokens_details.reasoning_tokens。
func TestExtractChatUsage_Thinking(t *testing.T) {
	tests := []struct {
		name string
		body string
		want map[string]int64
	}{
		{
			name: "openai_details_embedded",
			body: `{"usage":{"prompt_tokens":79,"completion_tokens":861,"total_tokens":940,
				"prompt_cache_hit_tokens":74,"prompt_cache_miss_tokens":5,
				"completion_tokens_details":{"reasoning_tokens":579}}}`,
			want: map[string]int64{
				"prompt_tokens": 79, "completion_tokens": 861,
				"cache_hit_tokens": 74, "cache_miss_tokens": 5,
				"thinking_tokens": 579,
			},
		},
		{
			// 顶层 thinking_tokens 优先于 details
			name: "top_level_thinking_wins",
			body: `{"usage":{"prompt_tokens":10,"completion_tokens":20,"thinking_tokens":7,
				"completion_tokens_details":{"reasoning_tokens":5}}}`,
			want: map[string]int64{
				"prompt_tokens": 10, "completion_tokens": 20,
				"cache_hit_tokens": 0, "cache_miss_tokens": 10,
				"thinking_tokens": 7,
			},
		},
		{
			// 商汤等：cached_tokens 兜底命中，miss 由 prompt-hit 推出
			name: "cached_tokens_fallback",
			body: `{"usage":{"prompt_tokens":100,"completion_tokens":50,
				"prompt_tokens_details":{"cached_tokens":60}}}`,
			want: map[string]int64{
				"prompt_tokens": 100, "completion_tokens": 50,
				"cache_hit_tokens": 60, "cache_miss_tokens": 40,
				"thinking_tokens": 0,
			},
		},
		{
			// 与 proxy.parseUsage 同口径：上游未返回缓存字段时按“全部未命中”记录
			// （miss = prompt），否则调试页显示“未命中 0”而请求日志显示等于输入，两处对不上。
			name: "miss_fallback_when_no_cache_fields",
			body: `{"usage":{"prompt_tokens":40,"completion_tokens":9}}`,
			want: map[string]int64{
				"prompt_tokens": 40, "completion_tokens": 9,
				"cache_hit_tokens": 0, "cache_miss_tokens": 40,
				"thinking_tokens": 0,
			},
		},
		{
			// 无思考、无缓存字段：思考保持 0；未命中须兜底为 prompt（与请求日志一致）
			name: "no_thinking",
			body: `{"usage":{"prompt_tokens":5,"completion_tokens":6}}`,
			want: map[string]int64{
				"prompt_tokens": 5, "completion_tokens": 6,
				"cache_hit_tokens": 0, "cache_miss_tokens": 5,
				"thinking_tokens": 0,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractChatUsage([]byte(tt.body))
			for k, want := range tt.want {
				if got[k] != want {
					t.Errorf("%s = %d, want %d（全部: %v）", k, got[k], want, got)
				}
			}
		})
	}
}

// TestExtractChatUsage_NoUsage 上游未返回 usage 时返回空表（前端显示 0，不 panic）。
func TestExtractChatUsage_NoUsage(t *testing.T) {
	if got := extractChatUsage([]byte(`{"choices":[]}`)); len(got) != 0 {
		t.Errorf("无 usage 时应返回空表，got %v", got)
	}
}
