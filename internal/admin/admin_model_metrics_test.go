package admin

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/icefairy/xuanji/internal/store"
)

// TestMetricsByModel 验证「纯模型」维度聚合：
// 同一上游真实模型挂在不同上游时必须合并成一行（看单个模型的真实用量与费用），
// 输入/输出（含思考）/缓存/耗时/费用各项口径正确。
func TestMetricsByModel(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	recs := []store.Record{
		// deepseek-chat：上游 A 一条成功，输入 1000（命中 800/未命中 200）、输出 100、思考 50
		{Timestamp: now, Upstream: "A", Model: "ds", UpstreamModel: "deepseek-chat",
			Status: 200, DurationMS: 2000, PromptTokens: 1000, CompletionTokens: 100, ThinkingTokens: 50,
			Tokens: 1150, PromptCacheHitTokens: 800, PromptCacheMissTokens: 200, Cost: 0.01},
		// deepseek-chat：上游 B 一条成功，输入 500（无缓存统计）、输出 40
		{Timestamp: now, Upstream: "B", Model: "ds", UpstreamModel: "deepseek-chat",
			Status: 200, DurationMS: 4000, PromptTokens: 500, CompletionTokens: 40,
			Tokens: 540, Cost: 0.02},
		// 另一个模型 + 一条失败
		{Timestamp: now, Upstream: "A", Model: "glm", UpstreamModel: "glm-4",
			Status: 500, DurationMS: 10},
	}
	if err := s.InsertBatch(recs); err != nil {
		t.Fatal(err)
	}

	h, hc := newTestHandler(t, testConfig())
	defer hc.Close()
	h.SetStore(s)

	rr := httptest.NewRecorder()
	h.MetricsByModel(rr, httptest.NewRequest("GET", "/admin/metrics/models?range=all", nil))
	var out []modelMetrics
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rr.Body.String())
	}
	if len(out) != 2 {
		t.Fatalf("want 2 rows (deepseek-chat, glm-4), got %d: %+v", len(out), out)
	}
	byName := map[string]modelMetrics{}
	for _, m := range out {
		byName[m.Model] = m
	}

	m := byName["deepseek-chat"]
	if m.Requests != 2 || m.Successes != 2 || m.Failures != 0 || m.SuccessRate != 1 {
		t.Errorf("deepseek-chat 请求统计错误: %+v", m)
	}
	if m.PromptTokens != 1500 {
		t.Errorf("prompt_tokens = %d, want 1500", m.PromptTokens)
	}
	if m.CompletionTokens != 140 || m.ThinkingTokens != 50 {
		t.Errorf("completion/thinking = %d/%d, want 140/50", m.CompletionTokens, m.ThinkingTokens)
	}
	if m.OutputTokens != 190 {
		t.Errorf("output_tokens = %d, want 190（输出含思考）", m.OutputTokens)
	}
	if m.TotalTokens != 1690 {
		t.Errorf("total_tokens = %d, want 1690", m.TotalTokens)
	}
	if m.CacheHitTokens != 800 || m.CacheMissTokens != 200 {
		t.Errorf("cache hit/miss = %d/%d, want 800/200", m.CacheHitTokens, m.CacheMissTokens)
	}
	if m.Cost < 0.0299 || m.Cost > 0.0301 {
		t.Errorf("cost = %v, want 0.03", m.Cost)
	}
	if m.SumDurationMS != 6000 || m.AvgLatencyMS != 3000 {
		t.Errorf("duration sum/avg = %d/%v, want 6000/3000", m.SumDurationMS, m.AvgLatencyMS)
	}

	// 失败模型：成功率 0，无 token
	if g := byName["glm-4"]; g.SuccessRate != 0 || g.Failures != 1 || g.TotalTokens != 0 {
		t.Errorf("glm-4 失败统计错误: %+v", g)
	}
}

// TestMetricsByModel_Range 验证时间范围过滤：今天的数据进 today，两天前的不进。
func TestMetricsByModel_Range(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	_ = s.InsertBatch([]store.Record{
		{Timestamp: now, Upstream: "A", UpstreamModel: "m-today", Status: 200, DurationMS: 100, PromptTokens: 10, CompletionTokens: 5, Tokens: 15},
		{Timestamp: now.AddDate(0, 0, -2), Upstream: "A", UpstreamModel: "m-old", Status: 200, DurationMS: 100, PromptTokens: 10, CompletionTokens: 5, Tokens: 15},
	})
	h, hc := newTestHandler(t, testConfig())
	defer hc.Close()
	h.SetStore(s)

	rr := httptest.NewRecorder()
	h.MetricsByModel(rr, httptest.NewRequest("GET", "/admin/metrics/models?range=today", nil))
	var out []modelMetrics
	json.Unmarshal(rr.Body.Bytes(), &out)
	if len(out) != 1 || out[0].Model != "m-today" {
		t.Fatalf("today 范围应只含 m-today: %+v", out)
	}
	if out[0].PromptTokens != 10 || out[0].OutputTokens != 5 {
		t.Errorf("m-today tokens 错误: %+v", out[0])
	}
}

// TestMetricsByModel_NilStore 验证无 store 时返回空数组而非报错。
func TestMetricsByModel_NilStore(t *testing.T) {
	h, hc := newTestHandler(t, testConfig())
	defer hc.Close()
	rr := httptest.NewRecorder()
	h.MetricsByModel(rr, httptest.NewRequest("GET", "/admin/metrics/models", nil))
	var out []modelMetrics
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("want empty, got %+v", out)
	}
}
