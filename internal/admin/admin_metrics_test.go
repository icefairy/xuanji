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
		wantTotal  int64
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

// TestMetricsHourly_TodayHours 验证「当天」维度按小时聚合：
// 返回东八区当天 0~23 时共 24 个桶（无数据补 0），昨天的数据不计入。
func TestMetricsHourly_TodayHours(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	loc := time.FixedZone("CST", 8*3600)
	now := time.Now().In(loc)
	at := func(hour int, dayOffset int) time.Time {
		return time.Date(now.Year(), now.Month(), now.Day(), hour, 30, 0, 0, loc).AddDate(0, 0, dayOffset)
	}
	// 今天 10 点 3 条（10 tokens），11 点 1 条（5 tokens）
	s.Insert(store.Record{Timestamp: at(10, 0), Upstream: "A", Model: "M", Status: 200, Tokens: 10})
	s.Insert(store.Record{Timestamp: at(10, 0), Upstream: "A", Model: "M", Status: 200, Tokens: 10})
	s.Insert(store.Record{Timestamp: at(10, 0), Upstream: "A", Model: "M", Status: 200, Tokens: 10})
	s.Insert(store.Record{Timestamp: at(11, 0), Upstream: "A", Model: "M", Status: 200, Tokens: 5})
	// 昨天 10 点 1 条（100 tokens）不应计入
	s.Insert(store.Record{Timestamp: at(10, -1), Upstream: "A", Model: "M", Status: 200, Tokens: 100})

	h, hc := newTestHandler(t, testConfig())
	defer hc.Close()
	h.SetStore(s)

	rr := httptest.NewRecorder()
	h.MetricsHourly(rr, httptest.NewRequest("GET", "/admin/metrics/hourly", nil))

	var out []hourlyBucket
	decodeBody(t, rr, &out)
	if len(out) != 24 {
		t.Fatalf("expected 24 hour buckets, got %d: %+v", len(out), out)
	}
	if out[0].Hour != "00:00" || out[23].Hour != "23:00" {
		t.Errorf("hour labels = %q ... %q, want 00:00 ... 23:00", out[0].Hour, out[23].Hour)
	}
	if out[10].Tokens != 30 || out[10].Requests != 3 {
		t.Errorf("hour 10 = %+v, want tokens=30 requests=3", out[10])
	}
	if out[11].Tokens != 5 || out[11].Requests != 1 {
		t.Errorf("hour 11 = %+v, want tokens=5 requests=1", out[11])
	}
	var total int64
	for _, b := range out {
		total += b.Tokens
	}
	if total != 35 {
		t.Errorf("total tokens = %d, want 35（昨天的 100 不应计入）", total)
	}
}
