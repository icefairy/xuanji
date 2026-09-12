package admin

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/icefairy/xuanji/internal/store"
)

// TestMetricsUpstreamModels 验证「上游 × 真实模型」聚合：
// 同一上游的两个模型必须分开统计（这正是本接口存在的原因），
// tokens/s 按输出 token（completion + thinking）/ 总耗时计算。
func TestMetricsUpstreamModels(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	// 上游 A：快模型 fast —— 1 条成功，输出 100 token / 1000ms = 100 tok/s
	// 上游 A：慢模型 slow —— 1 条成功，输出 50 token / 5000ms = 10 tok/s
	// 若只看上游 A 平均会把两者搅在一起（本接口就是要避免这点）
	recs := []store.Record{
		{Timestamp: now, Upstream: "A", Model: "client-fast", UpstreamModel: "fast",
			Status: 200, DurationMS: 1000, Tokens: 120, CompletionTokens: 100},
		{Timestamp: now, Upstream: "A", Model: "client-slow", UpstreamModel: "slow",
			Status: 200, DurationMS: 5000, Tokens: 70, CompletionTokens: 30, ThinkingTokens: 20},
		// 上游 B 一条失败记录（不应产生 tokens/s）
		{Timestamp: now, Upstream: "B", Model: "client-fast", UpstreamModel: "fast",
			Status: 500, DurationMS: 10, Tokens: 0},
	}
	if err := s.InsertBatch(recs); err != nil {
		t.Fatal(err)
	}

	h, hc := newTestHandler(t, testConfig())
	defer hc.Close()
	h.SetStore(s)

	rr := httptest.NewRecorder()
	h.MetricsUpstreamModels(rr, httptest.NewRequest("GET", "/admin/metrics/upstream-models?range=all", nil))
	var out []upstreamModelMetrics
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rr.Body.String())
	}
	if len(out) != 3 {
		t.Fatalf("want 3 rows (A/fast, A/slow, B/fast), got %d: %+v", len(out), out)
	}
	byKey := map[string]upstreamModelMetrics{}
	for _, m := range out {
		byKey[m.Upstream+"/"+m.Model] = m
	}

	if m := byKey["A/fast"]; m.TokensPerSec != 100 {
		t.Errorf("A/fast tokens_per_sec = %v, want 100", m.TokensPerSec)
	}
	if m := byKey["A/slow"]; m.TokensPerSec != 10 {
		t.Errorf("A/slow tokens_per_sec = %v, want 10", m.TokensPerSec)
	}
	// thinking token 计入输出（slow: 30+20=50 / 5s = 10）
	if m := byKey["A/slow"]; m.OutputTokens != 50 {
		t.Errorf("A/slow output_tokens = %v, want 50", m.OutputTokens)
	}
	if m := byKey["B/fast"]; m.TokensPerSec != 0 {
		t.Errorf("B/fast（失败）tokens_per_sec = %v, want 0", m.TokensPerSec)
	}
	if m := byKey["B/fast"]; m.SuccessRate != 0 || m.Failures != 1 {
		t.Errorf("B/fast 失败统计错误: %+v", m)
	}
}

// TestMetricsUpstreamModels_Filter 验证 ?upstream= 过滤只返回该上游的行。
func TestMetricsUpstreamModels_Filter(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	_ = s.InsertBatch([]store.Record{
		{Timestamp: now, Upstream: "A", UpstreamModel: "m1", Status: 200, DurationMS: 100, Tokens: 10, CompletionTokens: 10},
		{Timestamp: now, Upstream: "B", UpstreamModel: "m2", Status: 200, DurationMS: 100, Tokens: 10, CompletionTokens: 10},
	})
	h, hc := newTestHandler(t, testConfig())
	defer hc.Close()
	h.SetStore(s)

	rr := httptest.NewRecorder()
	h.MetricsUpstreamModels(rr, httptest.NewRequest("GET", "/admin/metrics/upstream-models?range=all&upstream=A", nil))
	var out []upstreamModelMetrics
	json.Unmarshal(rr.Body.Bytes(), &out)
	if len(out) != 1 || out[0].Upstream != "A" {
		t.Fatalf("filter failed: %+v", out)
	}
}

// TestMetricsUpstreamModels_NilStore 验证无 store 时返回空数组而非报错。
func TestMetricsUpstreamModels_NilStore(t *testing.T) {
	h, hc := newTestHandler(t, testConfig())
	defer hc.Close()
	rr := httptest.NewRecorder()
	h.MetricsUpstreamModels(rr, httptest.NewRequest("GET", "/admin/metrics/upstream-models", nil))
	if rr.Body.String() == "" {
		t.Fatal("empty body")
	}
	var out []upstreamModelMetrics
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
}
