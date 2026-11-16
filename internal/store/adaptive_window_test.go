package store

import (
	"testing"
	"time"
)

// 本文件是「统计窗口时间边界」的真实 SQLite 回归测试（TASK-window-fix.md）。
//
// 为什么必须走真实库：窗口下界曾经写成 datetime('now','-5 hours')，产出
// '2026-11-04 10:16:37'（空格分隔、无 Z），而 request_log.ts 存的是
// '2026-11-04T15:11:27Z'。TEXT 列做字符串比较时 'T'(0x54) > ' '(0x20)，
// 于是 `ts >= '2026-11-04 10:16:37'` 对任意 '2026-11-04T...' 恒为真，
// 5 小时窗口退化成全部历史（真实库实测 374 行 vs 正确 21 行）。
//
// 之前的测试只校验 Go 侧字符串相等、或直接用 time.Time 插入，从没让
// 「边界格式」和「ts 格式」在 SQL 里真正比较过，所以这个 bug 一路绿灯。

// insertRawLog 用原始 SQL 插入一行 request_log，ts 由调用方给定字面量。
// 不走 Store.Insert 是为了能精确控制 ts 的文本形态（含错误格式、空格格式等）。
func insertRawLog(t *testing.T, s *Store, ts, upstream, model string, status int) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO request_log
		(ts, upstream, model, endpoint, status, duration_ms, tokens)
		VALUES (?, ?, ?, 'chat', ?, 1, 1)`, ts, upstream, model, status); err != nil {
		t.Fatalf("insert raw log ts=%s: %v", ts, err)
	}
}

// TestAdaptiveSamplesRealSQLWindowBoundary 是本次缺陷的核心回归：
// 跨越 5 小时窗口边界的历史数据必须被排除，窗口内的必须计入。
func TestAdaptiveSamplesRealSQLWindowBoundary(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	fmtTs := func(d time.Duration) string { return now.Add(d).UTC().Format("2006-01-02T15:04:05Z") }

	// 窗口内（-1h、-3h、-4h59m）与窗口外（-5h1m、-8h、-20h）各插几条。
	inside := []time.Duration{-time.Hour, -3 * time.Hour, -(5*time.Hour - time.Minute)}
	outside := []time.Duration{-(5*time.Hour + time.Minute), -8 * time.Hour, -20 * time.Hour}
	for i, d := range inside {
		st := 200
		if i == 2 {
			st = 429
		}
		insertRawLog(t, s, fmtTs(d), "workbuddy", "m1", st)
	}
	for _, d := range outside {
		insertRawLog(t, s, fmtTs(d), "workbuddy", "m1", 429)
	}

	since := now.Add(-5 * time.Hour).UTC().Format(time.RFC3339)
	rows, err := s.AdaptiveSamples(since)
	if err != nil {
		t.Fatalf("AdaptiveSamples: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1: %+v", len(rows), rows)
	}
	// 只有窗口内的 3 条（200/200/429）→ total 3 ok 2
	if rows[0].Total != 3 || rows[0].OK != 2 {
		t.Fatalf("total/ok = %d/%d, want 3/2（窗口外的 %d 条历史必须排除）",
			rows[0].Total, rows[0].OK, len(outside))
	}
}

// TestAdaptiveSamplesRejectsWrongFormatSince 锁死「错误格式的 since 不得放大窗口」。
//
// 即使调用方（或历史遗留代码）把 since 传成 datetime() 的空格格式，
// store 层也必须先规整；否则跨日比较会让窗口外的历史全部命中。
func TestAdaptiveSamplesRejectsWrongFormatSince(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)

	// 今天凌晨 05:00（UTC）：在 5 小时窗口外，但字面量序号小于 "10:16:37" 的日子
	// 一旦下界退化成 '2026-11-04 10:16:37' 这类空格格式，它就会被错误计入。
	oldDay := now.Add(-22 * time.Hour)
	insertRawLog(t, s, oldDay.Format("2006-01-02T15:04:05Z"), "codebuddy", "m1", 429)
	insertRawLog(t, s, now.Add(-time.Minute).Format("2006-01-02T15:04:05Z"), "codebuddy", "m1", 200)

	wrongFormat := now.Add(-5 * time.Hour).UTC().Format("2006-01-02 15:04:05") // 空格、无 Z
	rows, err := s.AdaptiveSamples(wrongFormat)
	if err != nil {
		t.Fatalf("AdaptiveSamples: %v", err)
	}
	if len(rows) != 1 || rows[0].Total != 1 || rows[0].OK != 1 {
		t.Fatalf("rows = %+v, want total 1 ok 1（空格格式的下界必须被规整为 T...Z）", rows)
	}

	// 真正用 datetime() 写下的「错误窗口下界」不能反推出正确窗口，
	// 这里只断言：store 不会把时间点换算错（同一时刻的两种写法结果一致）。
	rows2, err := s.AdaptiveSamples(now.Add(-5 * time.Hour).UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("AdaptiveSamples: %v", err)
	}
	if len(rows2) != len(rows) || rows2[0].Total != rows[0].Total {
		t.Fatalf("RFC3339 与空格格式结果不一致：%+v vs %+v", rows, rows2)
	}
}

// TestAdaptiveProbeRowsWindowBoundary 覆盖 AdaptiveProbeRows 的 since 参数
// （它同样用 `ts >= ?`，TASK-window-fix.md 第 3 条）。
func TestAdaptiveProbeRowsWindowBoundary(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	fmtTs := func(d time.Duration) string { return now.Add(d).UTC().Format("2006-01-02T15:04:05Z") }

	insertRawLog(t, s, fmtTs(-time.Hour), "workbuddy", "m1", 200)   // 窗口内
	insertRawLog(t, s, fmtTs(-6*time.Hour), "workbuddy", "m1", 429) // 窗口外
	insertRawLog(t, s, fmtTs(-9*time.Hour), "workbuddy", "m1", 429) // 窗口外
	insertRawLog(t, s, fmtTs(-2*time.Hour), "other", "m1", 500)     // 不同上游
	insertRawLog(t, s, fmtTs(-2*time.Hour), "workbuddy", "m2", 429) // 不同模型
	since := now.Add(-5 * time.Hour).UTC().Format(time.RFC3339)

	rows, err := s.AdaptiveProbeRows("workbuddy", "m1", since, 100)
	if err != nil {
		t.Fatalf("AdaptiveProbeRows: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != 200 {
		t.Fatalf("rows = %+v, want 只有窗口内那 1 条 200", rows)
	}

	// 空格格式的 since 也要被规整（同样不得放大窗口）
	rows2, err := s.AdaptiveProbeRows("workbuddy", "m1", now.Add(-5*time.Hour).UTC().Format("2006-01-02 15:04:05"), 100)
	if err != nil {
		t.Fatalf("AdaptiveProbeRows(wrong format): %v", err)
	}
	if len(rows2) != 1 || rows2[0].Status != 200 {
		t.Fatalf("rows2 = %+v, want 1 条 200", rows2)
	}
}

// TestAdaptiveSamplesSinceWindowBoundary 覆盖试探期 after 的边界：
// after 落在窗口内时，窗口外、after 之前的行都不得计入。
func TestAdaptiveSamplesSinceWindowBoundary(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	fmtTs := func(d time.Duration) string { return now.Add(d).UTC().Format("2006-01-02T15:04:05Z") }

	insertRawLog(t, s, fmtTs(-8*time.Hour), "workbuddy", "m1", 429)    // 窗口外
	insertRawLog(t, s, fmtTs(-3*time.Hour), "workbuddy", "m1", 429)    // 窗口内、after 前
	insertRawLog(t, s, fmtTs(-30*time.Minute), "workbuddy", "m1", 200) // 窗口内、after 后
	insertRawLog(t, s, fmtTs(-20*time.Minute), "workbuddy", "m1", 200) // 窗口内、after 后

	since := now.Add(-5 * time.Hour).UTC().Format(time.RFC3339)
	after := now.Add(-time.Hour).UTC().Format(time.RFC3339)

	all, err := s.AdaptiveSamplesSince(since, "")
	if err != nil {
		t.Fatalf("AdaptiveSamplesSince: %v", err)
	}
	if len(all) != 1 || all[0].Total != 3 || all[0].OK != 2 {
		t.Fatalf("全窗口 = %+v, want total 3 ok 2", all)
	}

	probe, err := s.AdaptiveSamplesSince(since, after)
	if err != nil {
		t.Fatalf("AdaptiveSamplesSince(after): %v", err)
	}
	if len(probe) != 1 || probe[0].Total != 2 || probe[0].OK != 2 {
		t.Fatalf("试探口径 = %+v, want total 2 ok 2", probe)
	}

	// after 传空格格式同样要规整
	probe2, err := s.AdaptiveSamplesSince(since, now.Add(-time.Hour).UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		t.Fatalf("AdaptiveSamplesSince(wrong format after): %v", err)
	}
	if len(probe2) != 1 || probe2[0].Total != 2 || probe2[0].OK != 2 {
		t.Fatalf("试探口径(空格 after) = %+v, want total 2 ok 2", probe2)
	}
}

// TestAdaptiveWindowSinceSQLFormat 校验 SQL 侧窗口下界表达式的格式与语义。
// 核心断言：产出的字面量必须与 request_log.ts 同样是 'T...Z' 形态。
func TestAdaptiveWindowSinceSQLFormat(t *testing.T) {
	s := newTestStore(t)
	var got string
	if err := s.db.QueryRow(`SELECT ` + AdaptiveWindowSinceSQL(5)).Scan(&got); err != nil {
		t.Fatalf("query since expr: %v", err)
	}
	want := time.Now().UTC().Add(-5 * time.Hour).Format("2006-01-02T15:04:05Z")
	// 允许 2 秒执行误差
	gt, err := time.Parse("2006-01-02T15:04:05Z", got)
	if err != nil {
		t.Fatalf("窗口下界格式 = %q，无法按 '2006-01-02T15:04:05Z' 解析：%v", got, err)
	}
	if d := gt.Sub(mustParse(t, want)); d > 2*time.Second || d < -2*time.Second {
		t.Fatalf("窗口下界 = %s, want ≈ %s", got, want)
	}
	if got[10] != 'T' || got[len(got)-1] != 'Z' {
		t.Fatalf("窗口下界 = %q，必须带 T 与 Z（与 request_log.ts 同格式）", got)
	}

	// hours=0 → 约等于当前时刻；负数按 0 处理，不得放大窗口
	var zero, neg string
	_ = s.db.QueryRow(`SELECT ` + AdaptiveWindowSinceSQL(0)).Scan(&zero)
	_ = s.db.QueryRow(`SELECT ` + AdaptiveWindowSinceSQL(-3)).Scan(&neg)
	if zero != neg {
		t.Fatalf("hours=0 与 hours=-3 应等价（负数钳到 0），got %q vs %q", zero, neg)
	}
	if zt := mustParse(t, zero); time.Since(zt) > 3*time.Second {
		t.Fatalf("hours=0 的窗口下界 %s 偏离当前时刻过远", zero)
	}
}

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}
