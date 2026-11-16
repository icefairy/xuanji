package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/router"
)

// TestPickAvailableModel_Ordered 验证竖线映射按书写顺序取第一个可用（全局默认语义）。
func TestPickAvailableModel_Ordered(t *testing.T) {
	cfg := &config.Config{Upstreams: []config.Upstream{{
		Name: "up", Models: []string{"deepseek-v4-flash"},
		ModelMapping: map[string]string{
			"deepseek-v4-flash": "deepseek-v4.1-flash|deepseek-v4-flash",
		},
	}}}
	h := New(cfg, router.New(cfg), nil)
	ff := NewFastFailCache(time.Minute)
	h.SetFastFail(ff)
	up := &cfg.Upstreams[0]

	if got := h.pickAvailableModel(up, "deepseek-v4-flash"); got != "deepseek-v4.1-flash" {
		t.Fatalf("ordered pick = %q, want deepseek-v4.1-flash", got)
	}
	// 第一个被拉黑（配额用尽）→ 自动取第二个
	ff.MarkFailed("up", "deepseek-v4.1-flash")
	if got := h.pickAvailableModel(up, "deepseek-v4-flash"); got != "deepseek-v4-flash" {
		t.Fatalf("after first blacklisted pick = %q, want deepseek-v4-flash", got)
	}
	// 两个都拉黑 → 继续用最后一个候选（兜底项），避免白打已知用尽的第一个
	ff.MarkFailed("up", "deepseek-v4-flash")
	if got := h.pickAvailableModel(up, "deepseek-v4-flash"); got != "deepseek-v4-flash" {
		t.Fatalf("all blacklisted pick = %q, want last candidate deepseek-v4-flash", got)
	}
}

// TestMapModel_Ordered 验证 MapModel 按书写顺序取第一个（全局默认，多模型映射通用）。
func TestMapModel_Ordered(t *testing.T) {
	up := &config.Upstream{
		ModelMapping: map[string]string{"deepseek-v4-flash": "deepseek-v4.1-flash|deepseek-v4-flash"},
	}
	cfg := &config.Config{Upstreams: []config.Upstream{*up}}
	r := router.New(cfg)
	for i := 0; i < 20; i++ {
		if got := r.MapModel(up, "deepseek-v4-flash"); got != "deepseek-v4.1-flash" {
			t.Fatalf("ordered MapModel = %q, want deepseek-v4.1-flash", got)
		}
	}
	// 单值映射不受影响
	up2 := &config.Upstream{ModelMapping: map[string]string{"m": "only"}}
	if got := r.MapModel(up2, "m"); got != "only" {
		t.Fatalf("single-value MapModel = %q, want only", got)
	}
}

// TestAnyUpstreamGetsOrderedFallback 验证顺序语义对所有上游通用（无 per-upstream 开关）：
// 两个不同上游配置竖线映射，都应先打第一个模型。
func TestAnyUpstreamGetsOrderedFallback(t *testing.T) {
	cfg := &config.Config{Upstreams: []config.Upstream{
		{Name: "a", ModelMapping: map[string]string{"m": "a1|a2"}},
		{Name: "b", ModelMapping: map[string]string{"m": "b1|b2"}},
	}}
	h := New(cfg, router.New(cfg), nil)
	ff := NewFastFailCache(time.Minute)
	h.SetFastFail(ff)
	for i := range cfg.Upstreams {
		up := &cfg.Upstreams[i]
		want := up.ModelMapping["m"][:2] // "a1" / "b1"
		if got := h.pickAvailableModel(up, "m"); got != want {
			t.Fatalf("upstream %s pick = %q, want %q", up.Name, got, want)
		}
	}
}

// TestOrderedMapping_QuotaExhaustedFallsBackInSameUpstream 端到端验证核心需求：
// deepseek-v4-flash → "deepseek-v4.1-flash|deepseek-v4-flash"，
// 前者返回 429（配额用尽）时，同一上游内自动换后者，而不是跳到下一个上游。
func TestOrderedMapping_QuotaExhaustedFallsBackInSameUpstream(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(b, &req)
		mu.Lock()
		seen = append(seen, req.Model)
		mu.Unlock()
		if req.Model == "deepseek-v4.1-flash" {
			// 配额用尽：429 + 配额关键词
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"message":"You have exceeded your quota","type":"rate_limit_error"}}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"id":"ok","object":"chat.completion","model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Upstreams: []config.Upstream{{
			Name: "cb", BaseURL: upstream.URL, APIKey: "sk-test",
			Enabled: true, Tier: "free",
			Models:       []string{"deepseek-v4-flash"},
			ModelMapping: map[string]string{"deepseek-v4-flash": "deepseek-v4.1-flash|deepseek-v4-flash"},
		}},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules: []config.Rule{
				{Model: "deepseek-v4-flash", Upstreams: []string{"cb"}, Strategy: "primary_backup"},
			},
		},
		Retry: config.Retry{MaxRetries: 3, RetryStatuses: []int{429, 500, 502, 503, 504}},
	}
	h := New(cfg, router.New(cfg), nil)
	ff := NewFastFailCache(time.Minute)
	h.SetFastFail(ff)

	rec := doChat(t, h, `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("want 200 via same-upstream fallback, got %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	// 期望顺序：先试 4.1，429 后在同一上游换 v4-flash
	if len(got) < 2 {
		t.Fatalf("expected 2 attempts in same upstream, got %v", got)
	}
	if got[0] != "deepseek-v4.1-flash" || got[1] != "deepseek-v4-flash" {
		t.Fatalf("attempt order = %v, want [deepseek-v4.1-flash deepseek-v4-flash]", got)
	}
	// 配额用尽的真实模型应被拉黑，后续请求直接走第二个
	if !ff.IsBlacklisted("cb", "deepseek-v4.1-flash") {
		t.Fatal("exhausted real model should be blacklisted")
	}
}

// TestOrderedMapping_BothExhaustedDoesNotSpin 验证两个真实模型都用尽时不在同一上游空转：
// 第二次 429 后应停止降级（返回 false），交回正常 429 处理。
func TestOrderedMapping_BothExhaustedDoesNotSpin(t *testing.T) {
	up := &config.Upstream{
		Name:         "cb",
		ModelMapping: map[string]string{"m": "a|b"},
	}
	cfg := &config.Config{Upstreams: []config.Upstream{*up}}
	h := New(cfg, router.New(cfg), nil)
	ff := NewFastFailCache(time.Minute)
	h.SetFastFail(ff)
	body := []byte(`{"error":"quota exceeded"}`)

	// 第一个 a 用尽 → 降级到 b
	if !h.orderedModelFallbackAvailable(up, "m", "a", body) {
		t.Fatal("first exhaustion should fall back to b")
	}
	// b 也用尽 → 无其它可用候选，不再降级（避免空转重试）
	if h.orderedModelFallbackAvailable(up, "m", "b", body) {
		t.Fatal("second exhaustion should not spin in same upstream")
	}
}

// TestOrderedMapping_PlainRateLimitDoesNotFallback 回归：纯并发限流（无配额关键词）不降级模型。
func TestOrderedMapping_PlainRateLimitDoesNotFallback(t *testing.T) {
	up := &config.Upstream{
		Name:         "cb",
		ModelMapping: map[string]string{"m": "a|b"},
	}
	cfg := &config.Config{Upstreams: []config.Upstream{*up}}
	h := New(cfg, router.New(cfg), nil)
	ff := NewFastFailCache(time.Minute)
	h.SetFastFail(ff)

	if h.orderedModelFallbackAvailable(up, "m", "a", []byte(`{"error":"slow down"}`)) {
		t.Fatal("plain rate limit should not trigger model fallback")
	}
	if !h.orderedModelFallbackAvailable(up, "m", "a", []byte(`{"error":"quota exceeded"}`)) {
		t.Fatal("quota keyword should trigger model fallback")
	}
	if !ff.IsBlacklisted("cb", "a") {
		t.Fatal("fallback should blacklist the exhausted real model")
	}
}
