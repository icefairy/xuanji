package store

import (
	"testing"
	"time"
)

// TestTimeContextSessionRoundTrip 验证会话记录「写库 → 读回」时间精度与格式一致。
//
// 回归背景：last_seen 原本用 TEXT 存 time.Time，modernc.org/sqlite 写入与 Scan
// 的字符串格式不兼容，读回时报
// "unsupported Scan, storing driver.Value type string into type *time.Time"，
// 会话被静默丢弃 —— 表现为网关重启后所有会话的「距上轮」消失。
func TestTimeContextSessionRoundTrip(t *testing.T) {
	s := openTestStore(t)

	seen := time.Date(2026, 9, 10, 14, 5, 30, 0, time.UTC)
	rec := TimeContextRecord{
		ChainKey: "chain-abc",
		TailKey:  "tail-xyz",
		APIKey:   "key1",
		LastSeen: seen,
	}
	if err := s.SaveTimeContextEntries([]TimeContextRecord{rec}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := s.LoadTimeContextEntries(seen.Add(-time.Hour), 10)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("载入 %d 条, want 1", len(got))
	}
	if got[0].ChainKey != rec.ChainKey || got[0].TailKey != rec.TailKey {
		t.Fatalf("记录不符: %+v", got[0])
	}
	// 整数秒存储，比较时按秒对齐
	if !got[0].LastSeen.Truncate(time.Second).Equal(seen) {
		t.Fatalf("last_seen = %v, want %v", got[0].LastSeen, seen)
	}
}

// TestTimeContextSessionUpsert 同一 chain_key 重复写入应更新而非报错。
func TestTimeContextSessionUpsert(t *testing.T) {
	s := openTestStore(t)
	t1 := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	t2 := time.Now().UTC().Truncate(time.Second)

	if err := s.SaveTimeContextEntries([]TimeContextRecord{
		{ChainKey: "k", TailKey: "a", LastSeen: t1},
	}); err != nil {
		t.Fatalf("Save 1: %v", err)
	}
	if err := s.SaveTimeContextEntries([]TimeContextRecord{
		{ChainKey: "k", TailKey: "b", LastSeen: t2},
	}); err != nil {
		t.Fatalf("Save 2: %v", err)
	}

	got, err := s.LoadTimeContextEntries(t1.Add(-time.Hour), 10)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("行数 = %d, want 1（应 UPSERT）", len(got))
	}
	if got[0].TailKey != "b" {
		t.Fatalf("tail_key = %q, want b", got[0].TailKey)
	}
	if !got[0].LastSeen.Equal(t2) {
		t.Fatalf("last_seen = %v, want %v", got[0].LastSeen, t2)
	}

	n, err := s.CountTimeContextSessions()
	if err != nil || n != 1 {
		t.Fatalf("Count = %d, err=%v", n, err)
	}
}

// TestTimeContextSessionCutoffAndPrune 过期过滤与清理。
func TestTimeContextSessionCutoffAndPrune(t *testing.T) {
	s := openTestStore(t)
	old := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)
	fresh := time.Now().UTC().Truncate(time.Second)

	if err := s.SaveTimeContextEntries([]TimeContextRecord{
		{ChainKey: "old", LastSeen: old},
		{ChainKey: "fresh", LastSeen: fresh},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// cutoff 只留 24 小时内的
	got, err := s.LoadTimeContextEntries(fresh.Add(-24*time.Hour), 10)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0].ChainKey != "fresh" {
		t.Fatalf("cutoff 过滤不对: %+v", got)
	}

	n, err := s.PruneTimeContextSessions(fresh.Add(-24 * time.Hour))
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("删除 %d 行, want 1", n)
	}
	if left, _ := s.CountTimeContextSessions(); left != 1 {
		t.Fatalf("剩余 %d 行, want 1", left)
	}
}

// TestTimeContextSessionDelete 批量删除（LRU 淘汰同步落库用）。
func TestTimeContextSessionDelete(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	_ = s.SaveTimeContextEntries([]TimeContextRecord{
		{ChainKey: "a", LastSeen: now},
		{ChainKey: "b", LastSeen: now},
	})
	if err := s.DeleteTimeContextEntries([]string{"a"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, _ := s.LoadTimeContextEntries(now.Add(-time.Hour), 10)
	if len(got) != 1 || got[0].ChainKey != "b" {
		t.Fatalf("删除后剩余: %+v", got)
	}
}
