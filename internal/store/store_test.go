package store

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// openTestStore 在临时目录打开一个 store 供测试使用。
func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// countRows 返回 request_log 表当前行数。
func countRows(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM request_log`).Scan(&n); err != nil {
		t.Fatalf("COUNT(*): %v", err)
	}
	return n
}

// sampleRecord 构造一条最小可用测试记录。
func sampleRecord(i int) Record {
	return Record{
		Timestamp:        time.Date(2026, 8, 1, 12, 0, i, 0, time.UTC),
		Upstream:         "up-" + strconv.Itoa(i%3),
		Model:            "model-x",
		Endpoint:         "chat",
		Status:           200,
		DurationMS:       12,
		PromptTokens:     10,
		CompletionTokens: 24,
		Tokens:           34,
	}
}

func TestOpen_CreateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "xuanji.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// 数据文件应已创建
	if _, err := filepath.Glob(path); err != nil {
		t.Fatalf("glob db file: %v", err)
	}

	// request_log 表应存在
	var name string
	err = s.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='request_log'`,
	).Scan(&name)
	if err == sql.ErrNoRows {
		t.Fatal("request_log table not found")
	} else if err != nil {
		t.Fatalf("query table: %v", err)
	}

	// 两个索引也应存在
	for _, idx := range []string{"idx_request_log_ts", "idx_request_log_upstream"} {
		var iname string
		err = s.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, idx,
		).Scan(&iname)
		if err == sql.ErrNoRows {
			t.Errorf("index %s not found", idx)
		} else if err != nil {
			t.Fatalf("query index %s: %v", idx, err)
		}
	}
}

func TestInsertAndBatch(t *testing.T) {
	s := openTestStore(t)

	// 单条插入
	if err := s.Insert(sampleRecord(1)); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// 批量插入（事务）
	recs := make([]Record, 0, 5)
	for i := 0; i < 5; i++ {
		recs = append(recs, sampleRecord(i))
	}
	if err := s.InsertBatch(recs); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	if got := countRows(t, s); got != 6 {
		t.Errorf("rows = %d, want 6", got)
	}

	// 空批量不报错
	if err := s.InsertBatch(nil); err != nil {
		t.Fatalf("InsertBatch(nil): %v", err)
	}
}

func TestRecorder_AsyncWrite(t *testing.T) {
	s := openTestStore(t)
	r := NewRecorder(s)

	for i := 0; i < 10; i++ {
		r.Record(sampleRecord(i))
	}

	// Close 会 flush 剩余记录，10 条都应落库
	r.Close()
	if got := countRows(t, s); got != 10 {
		t.Errorf("rows after Close = %d, want 10", got)
	}
}

func TestRecorder_ChannelFullNoBlock(t *testing.T) {
	s := openTestStore(t)
	// 小缓冲 + 超长周期：后台协程不会在测试期间主动刷盘
	r := newRecorder(s, 3, time.Hour)

	// 先关闭后台协程，保证 channel 不再被 drain
	r.Close()

	// 塞满 channel（缓冲 3）
	for i := 0; i < 3; i++ {
		r.Record(sampleRecord(i))
	}

	// 第 4 条应被丢弃而非阻塞
	done := make(chan struct{})
	go func() {
		r.Record(sampleRecord(99))
		close(done)
	}()

	select {
	case <-done:
		// 未阻塞，符合预期
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Record blocked when channel full")
	}
}

func TestConfigCRUD(t *testing.T) {
	s := openTestStore(t)

	// 不存在的 key 返回空串
	if v, err := s.GetConfig("server.port"); err != nil || v != "" {
		t.Errorf("GetConfig(missing) = %q, %v; want empty, nil", v, err)
	}

	// 写入
	if err := s.SetConfig("server.port", "8787"); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if v, err := s.GetConfig("server.port"); err != nil || v != "8787" {
		t.Errorf("GetConfig = %q, %v; want 8787, nil", v, err)
	}

	// 更新（存在则覆盖）
	if err := s.SetConfig("server.port", "9000"); err != nil {
		t.Fatalf("SetConfig update: %v", err)
	}
	if v, _ := s.GetConfig("server.port"); v != "9000" {
		t.Errorf("GetConfig after update = %q, want 9000", v)
	}

	// GetAllConfig 返回全部
	if err := s.SetConfig("retry.max_retries", "3"); err != nil {
		t.Fatalf("SetConfig retry: %v", err)
	}
	all, err := s.GetAllConfig()
	if err != nil {
		t.Fatalf("GetAllConfig: %v", err)
	}
	if all["server.port"] != "9000" || all["retry.max_retries"] != "3" {
		t.Errorf("GetAllConfig = %v, want server.port=9000 retry.max_retries=3", all)
	}
}

func TestSeedDefaults(t *testing.T) {
	s := openTestStore(t)

	if err := s.SeedDefaults(); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}
	all, err := s.GetAllConfig()
	if err != nil {
		t.Fatalf("GetAllConfig: %v", err)
	}
	if len(all) != 7 {
		t.Errorf("defaults count = %d, want 7 (%v)", len(all), all)
	}
	if all["server.port"] != "8787" {
		t.Errorf("server.port = %q, want 8787", all["server.port"])
	}
	if all["retry.max_retries"] != "3" {
		t.Errorf("retry.max_retries = %q, want 3", all["retry.max_retries"])
	}
	if all["retry.fast_fail_minutes"] != "60" {
		t.Errorf("retry.fast_fail_minutes = %q, want 60", all["retry.fast_fail_minutes"])
	}
	if all["retry.fast_fail_probe_minutes"] != "35" {
		t.Errorf("retry.fast_fail_probe_minutes = %q, want 35", all["retry.fast_fail_probe_minutes"])
	}
	if all["retry.upstream_timeout"] != "60" {
		t.Errorf("retry.upstream_timeout = %q, want 60", all["retry.upstream_timeout"])
	}

	// 已有数据时不覆盖
	if err := s.SetConfig("server.port", "9999"); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if err := s.SeedDefaults(); err != nil {
		t.Fatalf("SeedDefaults second: %v", err)
	}
	if v, _ := s.GetConfig("server.port"); v != "9999" {
		t.Errorf("SeedDefaults overwrote existing server.port = %q, want 9999", v)
	}
}
