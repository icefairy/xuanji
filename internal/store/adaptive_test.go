package store

import (
	"testing"
	"time"
)

// TestAdaptiveWeightUpsertRoundTrip 覆盖建表幂等、upsert 覆盖、列表与删除。
func TestAdaptiveWeightUpsertRoundTrip(t *testing.T) {
	s := newTestStore(t)

	row := AdaptiveWeight{
		Upstream: "workbuddy", Model: "deepseek-v4.1-flash",
		Factor: 0.415, Rate: 0.249, Samples: 374, WindowHours: 5,
		ProbeAt: "2026-11-05T08:30:00Z", Backoff: 1, UpdatedAt: "2026-11-04T22:00:00Z",
	}
	if err := s.UpsertAdaptiveWeight(row); err != nil {
		t.Fatalf("upsert insert: %v", err)
	}
	rows, err := s.ListAdaptiveWeights()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].Upstream != "workbuddy" || rows[0].Model != "deepseek-v4.1-flash" {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].Factor != 0.415 || rows[0].Samples != 374 || rows[0].Backoff != 1 {
		t.Fatalf("row = %+v, want factor 0.415 samples 374 backoff 1", rows[0])
	}

	// upsert 覆盖（主键 upstream+model）
	row.Factor = 0.3
	row.Backoff = 2
	if err := s.UpsertAdaptiveWeight(row); err != nil {
		t.Fatalf("upsert update: %v", err)
	}
	rows, _ = s.ListAdaptiveWeights()
	if len(rows) != 1 || rows[0].Factor != 0.3 || rows[0].Backoff != 2 {
		t.Fatalf("rows = %+v, want 1 row factor 0.3 backoff 2", rows)
	}

	// 空串 model（上游级）也要能存
	if err := s.UpsertAdaptiveWeight(AdaptiveWeight{Upstream: "workbuddy", Model: "", Factor: 0.5}); err != nil {
		t.Fatalf("upsert empty model: %v", err)
	}
	rows, _ = s.ListAdaptiveWeights()
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}

	if err := s.DeleteAdaptiveWeight("workbuddy", "deepseek-v4.1-flash"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	rows, _ = s.ListAdaptiveWeights()
	if len(rows) != 1 || rows[0].Model != "" {
		t.Fatalf("rows after delete = %+v", rows)
	}
}

// TestAdaptiveSamplesFilter 校验失败判定口径（需求书第二节）：
// 只统计 2xx / 0 / 429 / >=500，客户端错误与 499 从分母剔除。
func TestAdaptiveSamplesFilter(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	ins := func(status int) {
		if err := s.Insert(Record{
			Timestamp: now.Add(-time.Minute), Upstream: "codebuddy", Model: "m1",
			Endpoint: "chat", Status: status,
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	for _, st := range []int{200, 204, 429, 500, 502, 0, 400, 401, 403, 404, 422, 499} {
		ins(st)
	}
	rows, err := s.AdaptiveSamples(now.Add(-time.Hour).Format(time.RFC3339))
	if err != nil {
		t.Fatalf("AdaptiveSamples: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	// 成功 200/204 = 2；真失败 429/500/502/0 = 4 → total 6
	if rows[0].Total != 6 || rows[0].OK != 2 {
		t.Fatalf("total/ok = %d/%d, want 6/2", rows[0].Total, rows[0].OK)
	}
}

// TestAdaptiveProbeRowsOrder 明细按时间升序（故障段分析依赖顺序）。
func TestAdaptiveProbeRowsOrder(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	for i, st := range []int{429, 200, 429, 400} {
		if err := s.Insert(Record{
			Timestamp: now.Add(time.Duration(i-10) * time.Minute),
			Upstream:  "workbuddy", Model: "m1", Endpoint: "chat", Status: st,
			ErrorDetail: "账号池无可用账号",
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	rows, err := s.AdaptiveProbeRows("workbuddy", "m1", now.Add(-24*time.Hour).Format(time.RFC3339), 100)
	if err != nil {
		t.Fatalf("AdaptiveProbeRows: %v", err)
	}
	// 400 不计入（只有 200/429/0）
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3: %+v", len(rows), rows)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].AtMS < rows[i-1].AtMS {
			t.Fatalf("rows 未按时间升序：%+v", rows)
		}
	}
	if rows[0].ErrText != "账号池无可用账号" {
		t.Fatalf("ErrText = %q, want 账号池无可用账号", rows[0].ErrText)
	}
}

// TestAdaptiveSamplesSince 校验试探期口径：after 非空时只统计该时刻之后的流量，
// 窗口内的历史 429 不得计入（TASK-probe-fix.md 的核心缺陷）。
func TestAdaptiveSamplesSince(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	ins := func(status int, ts time.Time) {
		if err := s.Insert(Record{
			Timestamp: ts, Upstream: "workbuddy", Model: "m1", Endpoint: "chat", Status: status,
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	// 试探起点之前：291 条全失败
	for i := 0; i < 291; i++ {
		ins(429, now.Add(-time.Hour))
	}
	// 试探起点之后：10 条全成功
	probeStart := now.Add(-10 * time.Minute)
	for i := 0; i < 10; i++ {
		ins(200, probeStart.Add(time.Duration(i)*time.Second))
	}
	since := now.Add(-5 * time.Hour).UTC().Format(time.RFC3339)

	all, err := s.AdaptiveSamples(since)
	if err != nil {
		t.Fatalf("AdaptiveSamples: %v", err)
	}
	if len(all) != 1 || all[0].Total != 301 || all[0].OK != 10 {
		t.Fatalf("全窗口 = %+v, want total 301 ok 10", all)
	}

	rows, err := s.AdaptiveSamplesSince(since, probeStart.UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("AdaptiveSamplesSince: %v", err)
	}
	if len(rows) != 1 || rows[0].Total != 10 || rows[0].OK != 10 {
		t.Fatalf("试探口径 = %+v, want total 10 ok 10（历史 429 必须排除）", rows)
	}

	// after 为空 → 与 AdaptiveSamples 等价
	rows, err = s.AdaptiveSamplesSince(since, "")
	if err != nil {
		t.Fatalf("AdaptiveSamplesSince(empty after): %v", err)
	}
	if len(rows) != 1 || rows[0].Total != 301 {
		t.Fatalf("空 after = %+v, want total 301", rows)
	}
}

// TestAdaptiveProbeStartColumn 校验 probe_start 列落库、往返与旧库自动加列。
func TestAdaptiveProbeStartColumn(t *testing.T) {
	s := newTestStore(t)
	row := AdaptiveWeight{
		Upstream: "workbuddy", Model: "m1", Factor: 0.3,
		ProbeAt: "2026-11-04T22:25:00Z", ProbeStart: "2026-11-04T22:15:00Z",
	}
	if err := s.UpsertAdaptiveWeight(row); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	rows, err := s.ListAdaptiveWeights()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].ProbeStart != row.ProbeStart {
		t.Fatalf("rows = %+v, want probe_start %s", rows, row.ProbeStart)
	}
}
