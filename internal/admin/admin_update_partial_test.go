package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/health"
	"github.com/icefairy/xuanji/internal/store"
)

// TestUpdateUpstream_PartialKeepsUnsentFields 回归测试（2026-08-27 硅基流动清空事故）：
// store.UpdateUpstream 是全列覆盖 UPDATE，若 PUT 只带个别字段（如 billing_exempt /
// per_model_billing 开关），未被提及的 base_url/api_key/models/model_mapping 等核心列
// 会被清成零值。修复后 handler 用 DB 旧行回填未传字段，稀疏 PUT 不再破坏上游配置；
// 显式传入的字段仍正常生效。
func TestUpdateUpstream_PartialKeepsUnsentFields(t *testing.T) {
	s, err := store.Open(t.TempDir() + "/xuanji.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	cfg := testConfig()
	cfg.Upstreams = append(cfg.Upstreams, config.Upstream{Name: "test-up", BaseURL: "http://unused", APIKey: "k"})
	hc := health.New(cfg)
	defer hc.Close()
	h := New(cfg, hc)
	h.SetStore(s)

	// 插入完整配置的上游
	full := &store.UpstreamRow{
		Name:            "test-up",
		Type:            "openai",
		BaseURL:         "https://api.example.com/v1",
		APIKey:          "sk-full-secret",
		Tier:            "payg",
		Priority:        5,
		Weight:          100,
		Models:          `["model-a","model-b"]`,
		ModelMapping:    `{"model-a":"server-a"}`,
		RequestOverride: `{"temperature":0.2}`,
	}
	if err := s.CreateUpstream(full); err != nil {
		t.Fatalf("create upstream: %v", err)
	}

	// 稀疏 PUT：只改 billing_exempt，其余字段不传
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/admin/upstreams/test-up",
		strings.NewReader(`{"billing_exempt":1}`))
	req.SetPathValue("name", "test-up")
	h.UpdateUpstream(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("update status = %d, body = %s", rr.Code, rr.Body.String())
	}

	got, err := s.GetUpstream("test-up")
	if err != nil {
		t.Fatalf("get upstream: %v", err)
	}
	// 未传字段必须保留
	if got.BaseURL != full.BaseURL {
		t.Errorf("base_url = %q, want %q（稀疏 PUT 不应清空）", got.BaseURL, full.BaseURL)
	}
	if got.APIKey != full.APIKey {
		t.Errorf("api_key 被清空/改写")
	}
	if got.Models != full.Models {
		t.Errorf("models = %q, want %q", got.Models, full.Models)
	}
	if got.ModelMapping != full.ModelMapping {
		t.Errorf("model_mapping 被清空/改写")
	}
	if got.Tier != full.Tier || got.Priority != full.Priority || got.Weight != full.Weight {
		t.Errorf("tier/priority/weight 被清空：%q/%d/%d", got.Tier, got.Priority, got.Weight)
	}
	if got.RequestOverride != full.RequestOverride {
		t.Errorf("request_override 被清空/改写")
	}
	// 显式传入的字段生效
	if got.BillingExempt != 1 {
		t.Errorf("billing_exempt = %d, want 1", got.BillingExempt)
	}
}

// TestUpdateUpstream_PartialExplicitFieldStillChanges 保证部分更新语义下显式传入的字段
// 仍可正常修改（回填只作用于未传字段）。
func TestUpdateUpstream_PartialExplicitFieldStillChanges(t *testing.T) {
	s, err := store.Open(t.TempDir() + "/xuanji.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	cfg := testConfig()
	cfg.Upstreams = append(cfg.Upstreams, config.Upstream{Name: "test-up", BaseURL: "http://unused", APIKey: "k"})
	hc := health.New(cfg)
	defer hc.Close()
	h := New(cfg, hc)
	h.SetStore(s)

	if err := s.CreateUpstream(&store.UpstreamRow{
		Name:     "test-up",
		Type:     "openai",
		BaseURL:  "https://old.example.com/v1",
		APIKey:   "sk-old",
		Tier:     "payg",
		Priority: 3,
		Weight:   50,
	}); err != nil {
		t.Fatalf("create upstream: %v", err)
	}

	// 显式改 base_url（未传的字段如 api_key/tier/priority 应保留）
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/admin/upstreams/test-up",
		strings.NewReader(`{"base_url":"https://new.example.com/v1"}`))
	req.SetPathValue("name", "test-up")
	h.UpdateUpstream(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("update status = %d, body = %s", rr.Code, rr.Body.String())
	}

	got, err := s.GetUpstream("test-up")
	if err != nil {
		t.Fatalf("get upstream: %v", err)
	}
	if got.BaseURL != "https://new.example.com/v1" {
		t.Errorf("base_url = %q, want 新值生效", got.BaseURL)
	}
	if got.APIKey != "sk-old" {
		t.Errorf("api_key 被清空/改写")
	}
	if got.Tier != "payg" || got.Priority != 3 || got.Weight != 50 {
		t.Errorf("tier/priority/weight 被清空")
	}
}

// TestUpdateUpstream_TimeoutRoundTripInList 回归测试（2026-09-10 编辑超时保存后回显 0）：
// GET /admin/upstreams 响应曾缺 timeout 字段，前端编辑表单回显 0，
// 再次保存时 parseInt||0 把已配置值误清零。保证列表响应回传 timeout。
func TestUpdateUpstream_TimeoutRoundTripInList(t *testing.T) {
	s, err := store.Open(t.TempDir() + "/xuanji.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	cfg := testConfig()
	cfg.Upstreams = append(cfg.Upstreams, config.Upstream{Name: "test-up", BaseURL: "http://unused", APIKey: "k"})
	hc := health.New(cfg)
	defer hc.Close()
	h := New(cfg, hc)
	h.SetStore(s)

	if err := s.CreateUpstream(&store.UpstreamRow{
		Name:    "test-up",
		Type:    "openai",
		BaseURL: "https://old.example.com/v1",
		APIKey:  "sk-old",
	}); err != nil {
		t.Fatalf("create upstream: %v", err)
	}

	// PUT 显式设 timeout=120
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/admin/upstreams/test-up",
		strings.NewReader(`{"timeout":120}`))
	req.SetPathValue("name", "test-up")
	h.UpdateUpstream(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("update status = %d, body = %s", rr.Code, rr.Body.String())
	}

	// GET 列表必须回传 timeout（前端编辑表单靠它回显）
	rr = httptest.NewRecorder()
	h.Upstreams(rr, httptest.NewRequest(http.MethodGet, "/admin/upstreams", nil))
	var out []upstreamResponse
	decodeBody(t, rr, &out)
	var found bool
	for _, u := range out {
		if u.Name == "test-up" {
			if u.Timeout != 120 {
				t.Errorf("timeout = %d, want 120（GET 列表必须回传）", u.Timeout)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("test-up 未出现在列表中")
	}

	// 稀疏 PUT（不带 timeout，如权重内联编辑 saveWeight）不得把 120 清掉
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/admin/upstreams/test-up",
		strings.NewReader(`{"weight":55}`))
	req.SetPathValue("name", "test-up")
	h.UpdateUpstream(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("sparse update status = %d, body = %s", rr.Code, rr.Body.String())
	}
	got, gerr := s.GetUpstream("test-up")
	if gerr != nil {
		t.Fatalf("get upstream: %v", gerr)
	}
	if got.Timeout != 120 {
		t.Errorf("稀疏 PUT 后 timeout = %d, want 120（未传应保留原值）", got.Timeout)
	}
	if got.Weight != 55 {
		t.Errorf("weight = %d, want 55", got.Weight)
	}
}
