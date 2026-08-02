package router

import (
	"errors"
	"testing"

	"github.com/icefairy/xuanji/internal/config"
)

func testConfig() *config.Config {
	return &config.Config{
		Upstreams: []config.Upstream{
			{Name: "primary", BaseURL: "http://p/v1", APIKey: "k", Priority: 30, Models: []string{"qwen3.6:35b"}},
			{Name: "backup", BaseURL: "http://b/v1", APIKey: "k", Priority: 10, Models: []string{"qwen3.6:35b"}},
			{Name: "silicon", BaseURL: "http://s/v1", APIKey: "k", Priority: 20,
				Models:       []string{"deepseek-v4-flash"},
				ModelMapping: map[string]string{"deepseek-v4-flash": "deepseek-ai/DeepSeek-V4-Flash"},
			},
		},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules: []config.Rule{
				{Model: "qwen*", Upstreams: []string{"primary", "backup"}, Strategy: "primary_backup"},
				{Model: "deepseek-v4-flash", Upstreams: []string{"silicon"}, Strategy: "primary_backup"},
			},
		},
	}
}

func TestRoute_WildcardMatch(t *testing.T) {
	r := New(testConfig())
	ups, _, err := r.Route("qwen3.6:35b")
	if err != nil {
		t.Fatalf("Route(qwen3.6:35b): %v", err)
	}
	if len(ups) != 2 {
		t.Fatalf("got %d upstreams, want 2", len(ups))
	}
}

func TestRoute_ExactMatch(t *testing.T) {
	r := New(testConfig())
	ups, _, err := r.Route("deepseek-v4-flash")
	if err != nil {
		t.Fatalf("Route(deepseek-v4-flash): %v", err)
	}
	if len(ups) != 1 || ups[0].Name != "silicon" {
		t.Errorf("got %v, want [silicon]", names(ups))
	}
}

func TestRoute_MultiUpstreamKeepsConfigOrder(t *testing.T) {
	r := New(testConfig())
	ups, _, err := r.Route("qwen3.6:35b")
	if err != nil {
		t.Fatalf("Route(qwen3.6:35b): %v", err)
	}
	// Route 只过滤不排序（排序由 proxy.selectCandidates 按 strategy 执行），保持配置顺序
	if ups[0].Name != "primary" || ups[1].Name != "backup" {
		t.Errorf("order = %v, want [primary backup] (config order)", names(ups))
	}
}

func TestRoute_FirstMatchingRuleWins(t *testing.T) {
	cfg := testConfig()
	// 两条规则都能匹配 m1，第一条应生效
	cfg.Routing.Rules = []config.Rule{
		{Model: "m*", Upstreams: []string{"backup"}, Strategy: "primary_backup"},
		{Model: "m1", Upstreams: []string{"primary"}, Strategy: "primary_backup"},
	}
	ups, _, err := New(cfg).Route("m1")
	if err != nil {
		t.Fatalf("Route(m1): %v", err)
	}
	if ups[0].Name != "backup" {
		t.Errorf("got %v, want rule order respected (first match = backup)", names(ups))
	}
}

func TestRoute_SkipsInvalidUpstreamNames(t *testing.T) {
	cfg := testConfig()
	cfg.Routing.Rules = []config.Rule{
		{Model: "ghost", Upstreams: []string{"missing", "primary"}, Strategy: "primary_backup"},
	}
	ups, _, err := New(cfg).Route("ghost")
	if err != nil {
		t.Fatalf("Route(ghost): %v", err)
	}
	if len(ups) != 1 || ups[0].Name != "primary" {
		t.Errorf("got %v, want [primary] with missing name skipped", names(ups))
	}
}

func TestRoute_NoMatch(t *testing.T) {
	r := New(testConfig())
	_, _, err := r.Route("unknown-model")
	if !errors.Is(err, ErrNoRoute) {
		t.Errorf("Route(unknown-model) error = %v, want ErrNoRoute", err)
	}
}

func TestRoute_AllUpstreamsInvalidContinues(t *testing.T) {
	cfg := testConfig()
	cfg.Routing.Rules = []config.Rule{
		{Model: "deepseek-v4-flash", Upstreams: []string{"missing"}, Strategy: "primary_backup"},
		{Model: "deepseek-v4-flash", Upstreams: []string{"silicon"}, Strategy: "primary_backup"},
	}
	ups, _, err := New(cfg).Route("deepseek-v4-flash")
	if err != nil {
		t.Fatalf("Route(deepseek-v4-flash): %v", err)
	}
	if ups[0].Name != "silicon" {
		t.Errorf("got %v, want [silicon] from second rule", names(ups))
	}
}

func TestMapModel_Applied(t *testing.T) {
	r := New(testConfig())
	silicon := r.upstreams["silicon"]

	if got := r.MapModel(silicon, "deepseek-v4-flash"); got != "deepseek-ai/DeepSeek-V4-Flash" {
		t.Errorf("MapModel(mapped) = %q, want deepseek-ai/DeepSeek-V4-Flash", got)
	}
	if got := r.MapModel(silicon, "unmapped-model"); got != "unmapped-model" {
		t.Errorf("MapModel(unmapped) = %q, want unchanged", got)
	}
	if got := r.MapModel(nil, "deepseek-v4-flash"); got != "deepseek-v4-flash" {
		t.Errorf("MapModel(nil) = %q, want unchanged", got)
	}
}

func TestMatchModel(t *testing.T) {
	cases := []struct {
		pattern string
		model   string
		want    bool
	}{
		{"qwen*", "qwen3.6:35b", true},
		{"qwen*", "deepseek-v4-flash", false},
		{"*", "anything:goes", true},
		{"deepseek-v4-flash", "deepseek-v4-flash", true},
		{"deepseek-v4-flash", "deepseek-v4-flashx", false},
		{"deepseek*flash", "deepseek-v4-flash", true},
		{"*flash", "deepseek-v4-flash", true},
		{"deepseek*", "deepseek-v4-flash", true},
		{"a*b*c", "a1b2c", true},
		{"a*b*c", "a1b2d", false},
	}
	for _, tc := range cases {
		if got := matchModel(tc.pattern, tc.model); got != tc.want {
			t.Errorf("matchModel(%q, %q) = %v, want %v", tc.pattern, tc.model, got, tc.want)
		}
	}
}

func names(ups []*config.Upstream) []string {
	out := make([]string, len(ups))
	for i, u := range ups {
		out[i] = u.Name
	}
	return out
}
