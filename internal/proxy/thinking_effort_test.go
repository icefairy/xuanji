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
	nb, changed := applyBestEffort(body, "mimo-v2.5", cfg)
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
	nb, changed := applyBestEffort(body, "mimo-v2.5", cfg)
	if !changed {
		t.Fatalf("应注入推荐值")
	}
	if got := gjson.GetBytes(nb, "reasoning_effort").String(); got != "medium" {
		t.Fatalf("期望 medium, got %s", got)
	}
	// 客户端已传 → auto 不覆盖
	body = []byte(`{"model":"mimo-v2.5","reasoning_effort":"low","messages":[]}`)
	nb, changed = applyBestEffort(body, "mimo-v2.5", cfg)
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
	nb, changed := applyBestEffort(body, "mimo-v2.5", cfg)
	if !changed {
		t.Fatalf("force 应覆盖")
	}
	if got := gjson.GetBytes(nb, "reasoning_effort").String(); got != "high" {
		t.Fatalf("期望 high, got %s", got)
	}
	// 未传 → auto 注入 recommended
	body = []byte(`{"model":"mimo-v2.5","messages":[]}`)
	nb, changed = applyBestEffort(body, "mimo-v2.5", cfg)
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
	nb, changed := applyBestEffort(body, "deepseek-v4-flash", cfg)
	if !changed {
		t.Fatalf("通配应匹配")
	}
	if got := gjson.GetBytes(nb, "reasoning_effort").String(); got != "high" {
		t.Fatalf("期望 high, got %s", got)
	}
	// sensenova-* 匹配 sensenova-6.7-flash-lite
	body = []byte(`{"model":"sensenova-6.7-flash-lite","messages":[]}`)
	nb, changed = applyBestEffort(body, "sensenova-6.7-flash-lite", cfg)
	if !changed {
		t.Fatalf("通配应匹配")
	}
	if got := gjson.GetBytes(nb, "reasoning_effort").String(); got != "low" {
		t.Fatalf("期望 low, got %s", got)
	}
	// 无匹配 → 不修改
	body = []byte(`{"model":"unknown-model","messages":[]}`)
	if nb, changed = applyBestEffort(body, "unknown-model", cfg); changed {
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
