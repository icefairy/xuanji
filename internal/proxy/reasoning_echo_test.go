package proxy

import "testing"

func TestNeedsReasoningEcho(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		// deepseek：硬要求
		{"deepseek-v4-flash", true},
		{"DeepSeek-V4-Pro", true},
		// glm：软要求（官方建议回传）
		{"glm-5.3-flash", true},
		{"glm-4.6", true},
		{"glm4:9b", true},
		{"GLM-5", true},
		// mimo：官方文档要求（静默降级）
		{"mimo-v2.6-flash", true},
		{"mimo-v2.6-flash-free", true},
		{"MiMo-V2.5", true},
		// 不注入：未知上游可能严格校验未知字段
		{"kimi-k3", false},
		{"qwen3.8-flash", false},
		{"gpt-5", false},
		{"hy3", false},
		{"", false},
	}
	for _, c := range cases {
		if got := needsReasoningEcho(c.model); got != c.want {
			t.Errorf("needsReasoningEcho(%q) = %v, want %v", c.model, got, c.want)
		}
	}
}
