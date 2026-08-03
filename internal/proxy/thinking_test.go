package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// normalizeEffort 调用 normalizeThinkingEffort 并返回转换后的 body 字符串。
func normalizeEffort(t *testing.T, body, upstreamModel string) (string, bool) {
	t.Helper()
	nb, changed := normalizeThinkingEffort([]byte(body), upstreamModel)
	if nb == nil {
		t.Fatalf("normalizeThinkingEffort returned nil body for %q", upstreamModel)
	}
	return string(nb), changed
}

func TestNormalizeThinkingEffort_NoEffort_NoChange(t *testing.T) {
	body := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`
	nb, changed := normalizeThinkingEffort([]byte(body), "deepseek-v4-flash")
	if changed {
		t.Fatalf("no reasoning_effort should not change body, got changed=true")
	}
	if string(nb) != body {
		t.Fatalf("body should be identical, got %s", nb)
	}
}

func TestNormalizeThinkingEffort_DeepSeek(t *testing.T) {
	cases := []struct {
		name, effort, want string
		wantReasoning      bool // 是否保留 reasoning_effort 字段
	}{
		{"none → thinking disabled", "none", "disabled", false},
		{"low → low", "low", "", true},
		{"medium → high", "medium", "high", true},
		{"high → high", "high", "high", true},
		{"max → max", "max", "max", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"` + tc.effort + `"}`
			nb, changed := normalizeEffort(t, body, "deepseek-v4-flash")
			if !changed {
				t.Fatalf("expected changed=true")
			}
			if gjson.Get(nb, "reasoning_effort").Exists() != tc.wantReasoning {
				t.Fatalf("reasoning_effort exists=%v, want %v (body=%s)", gjson.Get(nb, "reasoning_effort").Exists(), tc.wantReasoning, nb)
			}
			if tc.want == "disabled" {
				if got := gjson.Get(nb, "thinking.type").String(); got != "disabled" {
					t.Fatalf("thinking.type=%q, want disabled (body=%s)", got, nb)
				}
			}
			if tc.wantReasoning && tc.want != "" {
				if got := gjson.Get(nb, "reasoning_effort").String(); got != tc.want {
					t.Fatalf("reasoning_effort=%q, want %q (body=%s)", got, tc.want, nb)
				}
			}
		})
	}
}

func TestNormalizeThinkingEffort_DeepSeekPro_LowRaisesToHigh(t *testing.T) {
	// deepseek-v4-pro 官方映射：low 档抬到 high
	body := `{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`
	nb, changed := normalizeEffort(t, body, "deepseek-v4-pro")
	if !changed {
		t.Fatalf("expected changed")
	}
	if got := gjson.Get(nb, "reasoning_effort").String(); got != "high" {
		t.Fatalf("pro low should map to high, got %q (body=%s)", got, nb)
	}
}

func TestNormalizeThinkingEffort_SenseNova(t *testing.T) {
	cases := []struct {
		name, effort, wantEffort string
		wantDisabled             bool
	}{
		{"none → thinking disabled", "none", "", true},
		{"low → output_config low", "low", "low", false},
		{"medium → output_config medium", "medium", "medium", false},
		{"high → output_config high", "high", "high", false},
		{"max → high (商汤最高档)", "max", "high", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"sensenova-6.7-flash-lite","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"` + tc.effort + `"}`
			nb, changed := normalizeEffort(t, body, "sensenova-6.7-flash-lite")
			if !changed {
				t.Fatalf("expected changed=true")
			}
			if gjson.Get(nb, "reasoning_effort").Exists() {
				t.Fatalf("reasoning_effort should be removed for sensenova (body=%s)", nb)
			}
			if tc.wantDisabled {
				if got := gjson.Get(nb, "thinking.type").String(); got != "disabled" {
					t.Fatalf("thinking.type=%q, want disabled (body=%s)", got, nb)
				}
			} else {
				if got := gjson.Get(nb, "output_config.effort").String(); got != tc.wantEffort {
					t.Fatalf("output_config.effort=%q, want %q (body=%s)", got, tc.wantEffort, nb)
				}
			}
		})
	}
}

func TestNormalizeThinkingEffort_KimiK3(t *testing.T) {
	body := `{"model":"kimi-k3","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium"}`
	nb, changed := normalizeEffort(t, body, "kimi-k3")
	if !changed {
		t.Fatalf("expected changed")
	}
	if got := gjson.Get(nb, "reasoning_effort").String(); got != "high" {
		t.Fatalf("k3 medium should map to high, got %q (body=%s)", got, nb)
	}
	// K3 始终思考：none 映射到 low 而不是关闭
	body2 := `{"model":"kimi-k3","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`
	nb2, _ := normalizeEffort(t, body2, "kimi-k3")
	if got := gjson.Get(nb2, "reasoning_effort").String(); got != "low" {
		t.Fatalf("k3 none should map to low (cannot disable), got %q (body=%s)", got, nb2)
	}
}

func TestNormalizeThinkingEffort_KimiK2AndGLM(t *testing.T) {
	for _, model := range []string{"kimi-k2.6", "glm-4.5"} {
		// 有强度 → thinking enabled
		body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`
		nb, changed := normalizeEffort(t, body, model)
		if !changed {
			t.Fatalf("%s: expected changed", model)
		}
		if gjson.Get(nb, "reasoning_effort").Exists() {
			t.Fatalf("%s: reasoning_effort should be removed (body=%s)", model, nb)
		}
		if got := gjson.Get(nb, "thinking.type").String(); got != "enabled" {
			t.Fatalf("%s: thinking.type=%q, want enabled (body=%s)", model, got, nb)
		}
		// none → thinking disabled
		body2 := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`
		nb2, _ := normalizeEffort(t, body2, model)
		if got := gjson.Get(nb2, "thinking.type").String(); got != "disabled" {
			t.Fatalf("%s: none should disable thinking, got %q (body=%s)", model, got, nb2)
		}
	}
}

func TestNormalizeThinkingEffort_OpenAINative_Passthrough(t *testing.T) {
	// o3/o4/gpt-5 原生支持 reasoning_effort → 透传不改
	for _, model := range []string{"o3-mini", "o4-mini", "gpt-5.6"} {
		body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium"}`
		nb, changed := normalizeThinkingEffort([]byte(body), model)
		if changed {
			t.Fatalf("%s: openai-native should passthrough, got changed", model)
		}
		if string(nb) != body {
			t.Fatalf("%s: body should be identical, got %s", model, nb)
		}
	}
}

func TestNormalizeThinkingEffort_UnknownModel_Passthrough(t *testing.T) {
	body := `{"model":"unknown-model-x","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`
	nb, changed := normalizeThinkingEffort([]byte(body), "unknown-model-x")
	if changed || string(nb) != body {
		t.Fatalf("unknown model should passthrough unchanged, changed=%v body=%s", changed, nb)
	}
}

// TestNormalizeThinkingEffort_KeepsOtherFields 确保转换不破坏其他请求字段。
func TestNormalizeThinkingEffort_KeepsOtherFields(t *testing.T) {
	body := `{"model":"sensenova-6.7-flash-lite","messages":[{"role":"user","content":"hi"}],"temperature":0.7,"max_tokens":4096,"reasoning_effort":"low"}`
	nb, changed := normalizeEffort(t, body, "sensenova-6.7-flash-lite")
	if !changed {
		t.Fatalf("expected changed")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(nb), &m); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if m["temperature"] != 0.7 {
		t.Fatalf("temperature lost: %v", m["temperature"])
	}
	if !strings.Contains(nb, `"max_tokens":4096`) {
		t.Fatalf("max_tokens lost: %s", nb)
	}
	if got := gjson.Get(nb, "output_config.effort").String(); got != "low" {
		t.Fatalf("output_config.effort=%q, want low", got)
	}
}
