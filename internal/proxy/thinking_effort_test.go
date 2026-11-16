package proxy

import (
	"testing"

	"github.com/tidwall/gjson"

	"github.com/icefairy/xuanji/internal/config"
)

func testEffortCfg(auto, force bool) *config.Config {
	return &config.Config{
		Proxy: config.Proxy{
			AutoBestEffort:  auto,
			ForceBestEffort: force,
			EffortConfigs: []config.EffortConfig{
				{Model: "mimo-v2.5", Recommended: "medium", Forced: "high"},
				{Model: "deepseek-*", Recommended: "high"},
				{Model: "sensenova-*", Recommended: "low", Forced: "medium"},
			},
		},
	}
}

func TestApplyBestEffort_Off(t *testing.T) {
	cfg := testEffortCfg(false, false)
	body := []byte(`{"model":"mimo-v2.5","messages":[]}`)
	nb, changed := applyBestEffort(body, "mimo-v2.5", "mimo-v2.5", cfg)
	if changed {
		t.Fatalf("开关全关不应修改 body")
	}
	if string(nb) != string(body) {
		t.Fatalf("body 不应变化: %s", nb)
	}
}

func TestApplyBestEffort_AutoInject(t *testing.T) {
	cfg := testEffortCfg(true, false)
	// 客户端未传 → 注入推荐值 medium
	body := []byte(`{"model":"mimo-v2.5","messages":[]}`)
	nb, changed := applyBestEffort(body, "mimo-v2.5", "mimo-v2.5", cfg)
	if !changed {
		t.Fatalf("应注入推荐值")
	}
	if got := gjson.GetBytes(nb, "reasoning_effort").String(); got != "medium" {
		t.Fatalf("期望 medium, got %s", got)
	}
	// 客户端已传 → auto 不覆盖
	body = []byte(`{"model":"mimo-v2.5","reasoning_effort":"low","messages":[]}`)
	nb, changed = applyBestEffort(body, "mimo-v2.5", "mimo-v2.5", cfg)
	if changed {
		t.Fatalf("auto 模式不应覆盖客户端传值")
	}
	if got := gjson.GetBytes(nb, "reasoning_effort").String(); got != "low" {
		t.Fatalf("期望保留 low, got %s", got)
	}
}

func TestApplyBestEffort_ForceOverride(t *testing.T) {
	cfg := testEffortCfg(true, true)
	// 客户端已传 low → 强制覆盖为 high
	body := []byte(`{"model":"mimo-v2.5","reasoning_effort":"low","messages":[]}`)
	nb, changed := applyBestEffort(body, "mimo-v2.5", "mimo-v2.5", cfg)
	if !changed {
		t.Fatalf("force 应覆盖")
	}
	if got := gjson.GetBytes(nb, "reasoning_effort").String(); got != "high" {
		t.Fatalf("期望 high, got %s", got)
	}
	// 未传 → auto 注入 recommended
	body = []byte(`{"model":"mimo-v2.5","messages":[]}`)
	nb, changed = applyBestEffort(body, "mimo-v2.5", "mimo-v2.5", cfg)
	if !changed {
		t.Fatalf("auto 应注入")
	}
	if got := gjson.GetBytes(nb, "reasoning_effort").String(); got != "medium" {
		t.Fatalf("期望 medium, got %s", got)
	}
}

func TestApplyBestEffort_Wildcard(t *testing.T) {
	cfg := testEffortCfg(true, false)
	// deepseek-* 匹配 deepseek-v4-flash
	body := []byte(`{"model":"deepseek-v4-flash","messages":[]}`)
	nb, changed := applyBestEffort(body, "deepseek-v4-flash", "deepseek-v4-flash", cfg)
	if !changed {
		t.Fatalf("通配应匹配")
	}
	if got := gjson.GetBytes(nb, "reasoning_effort").String(); got != "high" {
		t.Fatalf("期望 high, got %s", got)
	}
	// sensenova-* 匹配 sensenova-6.7-flash-lite
	body = []byte(`{"model":"sensenova-6.7-flash-lite","messages":[]}`)
	nb, changed = applyBestEffort(body, "sensenova-6.7-flash-lite", "sensenova-6.7-flash-lite", cfg)
	if !changed {
		t.Fatalf("通配应匹配")
	}
	if got := gjson.GetBytes(nb, "reasoning_effort").String(); got != "low" {
		t.Fatalf("期望 low, got %s", got)
	}
	// 无匹配 → 不修改
	body = []byte(`{"model":"unknown-model","messages":[]}`)
	if nb, changed = applyBestEffort(body, "unknown-model", "unknown-model", cfg); changed {
		t.Fatalf("无匹配不应修改")
	}
}

func TestMatchEffortPattern(t *testing.T) {
	cases := []struct {
		pattern, model string
		want           bool
	}{
		{"mimo-v2.5", "mimo-v2.5", true},
		{"deepseek-*", "deepseek-v4-flash", true},
		{"deepseek-*", "deepseek-v4-pro", true},
		{"deepseek-*", "mimo-v2.5", false},
		{"*", "anything", true},
		{"sensenova-*", "sensenova-6.7-flash-lite", true},
		{"sensenova-*", "SENSENOVA-6.7", false}, // 大小写敏感，与路由一致
		{"glm-*", "glm-4.5", true},
		{"glm-*", "glm4.5", false},
	}
	for _, c := range cases {
		if got := matchEffortPattern(c.pattern, c.model); got != c.want {
			t.Fatalf("matchEffortPattern(%q,%q)=%v want %v", c.pattern, c.model, got, c.want)
		}
	}
}

// upstreamModel（映射后真实模型名）优先于客户端模型名匹配 effort 配置。
// 实际故障场景：客户端传聚合名 flash → model_mapping 映射为 agnes-2.5-flash →
// 用户按 agnes-2.5-flash 配置 recommended=high，旧行为拿 flash 匹配不上导致不注入。
func TestApplyBestEffort_UpstreamModelMatch(t *testing.T) {
	cfg := &config.Config{
		Proxy: config.Proxy{
			AutoBestEffort: true,
			EffortConfigs: []config.EffortConfig{
				{Model: "agnes-2.5-flash", Recommended: "high"},
			},
		},
	}
	// 客户端 model=flash 无法直接命中，靠 upstreamModel 命中
	body := []byte(`{"model":"flash","messages":[]}`)
	nb, changed := applyBestEffort(body, "flash", "agnes-2.5-flash", cfg)
	if !changed {
		t.Fatalf("upstreamModel 应命中配置并注入")
	}
	if got := gjson.GetBytes(nb, "reasoning_effort").String(); got != "high" {
		t.Fatalf("期望 high, got %s", got)
	}
}

// upstreamModel 命中的配置优先于客户端 model 命中的配置（配置顺序靠前者胜出）。
func TestApplyBestEffort_UpstreamModelPriority(t *testing.T) {
	cfg := &config.Config{
		Proxy: config.Proxy{
			AutoBestEffort: true,
			EffortConfigs: []config.EffortConfig{
				{Model: "agnes-2.5-flash", Recommended: "medium"},
				{Model: "flash", Recommended: "low"},
			},
		},
	}
	// 两条都能命中（upstreamModel 命中第一条、model 命中第二条），应取 upstreamModel 命中的那条
	body := []byte(`{"model":"flash","messages":[]}`)
	nb, changed := applyBestEffort(body, "flash", "agnes-2.5-flash", cfg)
	if !changed {
		t.Fatalf("应命中配置并注入")
	}
	if got := gjson.GetBytes(nb, "reasoning_effort").String(); got != "medium" {
		t.Fatalf("期望 upstreamModel 优先命中 medium, got %s", got)
	}
}

// agnes-2.5-* 被识别为 agnes profile，xhigh → max（agnes 枚举无 xhigh，否则 400）。
func TestNormalizeThinkingEffort_Agnes(t *testing.T) {
	profiles := []struct{ model, effort, wantEffort string }{
		{"agnes-2.5-flash", "xhigh", "max"},
		{"agnes-2.5-pro", "xhigh", "max"},
		{"agnes-2.5-flash", "high", "high"},
		{"agnes-2.5-pro", "low", "low"},
	}
	for _, p := range profiles {
		if got := matchThinkingProfile(p.model); got != "agnes" {
			t.Fatalf("matchThinkingProfile(%q)=%q want agnes", p.model, got)
		}
		body := []byte(`{"model":"` + p.model + `","reasoning_effort":"` + p.effort + `"}`)
		out, changed := normalizeThinkingEffort(body, p.model)
		got := gjson.GetBytes(out, "reasoning_effort").String()
		if changed && got != p.wantEffort {
			t.Fatalf("normalizeThinkingEffort(%s, effort=%s)=%s want %s", p.model, p.effort, got, p.wantEffort)
		}
		if !changed && p.effort != p.wantEffort {
			t.Fatalf("expected change for %s effort=%s", p.model, p.effort)
		}
	}
}

// mimo-* 被识别为 mimo profile，xhigh/max → high（mimo 枚举只到 high，否则 400）。
func TestNormalizeThinkingEffort_Mimo(t *testing.T) {
	profiles := []struct{ model, effort, wantEffort string }{
		{"mimo-v2.5", "xhigh", "high"},
		{"mimo-v2.5-pro", "max", "high"},
		{"mimo-v2.5", "high", "high"},
		{"mimo-v2.5-pro", "low", "low"},
	}
	for _, p := range profiles {
		if got := matchThinkingProfile(p.model); got != "mimo" {
			t.Fatalf("matchThinkingProfile(%q)=%q want mimo", p.model, got)
		}
		body := []byte(`{"model":"` + p.model + `","reasoning_effort":"` + p.effort + `"}`)
		out, changed := normalizeThinkingEffort(body, p.model)
		got := gjson.GetBytes(out, "reasoning_effort").String()
		if changed && got != p.wantEffort {
			t.Fatalf("normalizeThinkingEffort(%s, effort=%s)=%s want %s", p.model, p.effort, got, p.wantEffort)
		}
		if !changed && p.effort != p.wantEffort {
			t.Fatalf("expected change for %s effort=%s", p.model, p.effort)
		}
	}
}
