package admin

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/icefairy/xuanji/internal/store"
)

// TestMetricsRange 验证时间范围参数过滤正确：today / 3d / 7d / 30d / all。
func TestMetricsRange(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	// 1 条今天（东八区当天）、2 条 2 天前、2 条 29 天前 → 共 5 条
	for i := 0; i < 1; i++ {
		s.Insert(store.Record{Timestamp: now, Upstream: "A", Model: "M", Status: 200, DurationMS: 1, Tokens: 10})
	}
	for i := 0; i < 2; i++ {
		s.Insert(store.Record{Timestamp: now.AddDate(0, 0, -2), Upstream: "B", Model: "M", Status: 200, DurationMS: 2, Tokens: 20})
	}
	for i := 0; i < 2; i++ {
		s.Insert(store.Record{Timestamp: now.AddDate(0, 0, -29), Upstream: "C", Model: "M", Status: 500, DurationMS: 3, Tokens: 30})
	}

	h, hc := newTestHandler(t, testConfig())
	defer hc.Close()
	h.SetStore(s)

	cases := []struct {
		rangeParam string
		wantTotal int64
	}{
		{"today", 1},
		{"3d", 3},  // 今天 + 2 天前
		{"7d", 3},  // 同上
		{"30d", 5}, // 全部
		{"all", 5},
	}
	for _, c := range cases {
		t.Run(c.rangeParam, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h.MetricsSummary(rr, httptest.NewRequest("GET", "/admin/metrics/summary?range="+c.rangeParam, nil))
			var out struct {
				TotalRequests int64 `json:"total_requests"`
			}
			json.Unmarshal(rr.Body.Bytes(), &out)
			if out.TotalRequests != c.wantTotal {
				t.Errorf("range=%s total = %d, want %d", c.rangeParam, out.TotalRequests, c.wantTotal)
			}
		})
	}
}