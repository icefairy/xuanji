package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestOpen_AllPooledConnectionsBusyTimeout 回归测试：
// PRAGMA busy_timeout 必须应用到连接池中的每一个连接（通过 DSN _pragma），
// 而不是只对池中第一条连接 Exec。
//
// 根因背景：database/sql 是连接池，并发时会产生多条底层连接；busy_timeout 是
// 【每连接】设置。旧实现用 db.Exec 设 PRAGMA 只作用于第一条连接，其余并发连接
// 保持 busy_timeout=0，写锁冲突立即抛 SQLITE_BUSY（线上 daily_stats upsert
// 报 “database is locked (5) (SQLITE_BUSY)”）。
func TestOpen_AllPooledConnectionsBusyTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	db := s.DB()

	const n = 8 // 同时持有 8 条连接，强制池创建 8 个底层连接
	conns := make([]*sql.Conn, 0, n)
	for i := 0; i < n; i++ {
		c, err := db.Conn(context.Background())
		if err != nil {
			t.Fatalf("Conn %d: %v", i, err)
		}
		conns = append(conns, c)
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	for i, c := range conns {
		var bt int
		if err := c.QueryRowContext(context.Background(), `PRAGMA busy_timeout`).Scan(&bt); err != nil {
			t.Fatalf("conn %d: query busy_timeout: %v", i, err)
		}
		if bt < 5000 {
			t.Errorf("conn %d busy_timeout=%d, want >= 5000（说明该连接未应用 busy_timeout 参数）", i, bt)
		}
		var mode string
		if err := c.QueryRowContext(context.Background(), `PRAGMA journal_mode`).Scan(&mode); err != nil {
			t.Fatalf("conn %d: query journal_mode: %v", i, err)
		}
		if strings.ToLower(mode) != "wal" {
			t.Errorf("conn %d journal_mode=%q, want wal", i, mode)
		}
	}
}

// TestConcurrentWrites_NoSQLiteBusy 时序复现测试：
// 多个 goroutine 各自独占一条池连接，做带缓冲窗口的并发写入，制造真实的
// 写锁冲突。修复前（无 busy_timeout 的连接）会立即 SQLITE_BUSY，修复后能
// 在 busy_timeout 内等锁重试并全部成功。
func TestConcurrentWrites_NoSQLiteBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	const writers = 6
	const iters = 25
	ctx := context.Background()

	errCh := make(chan error, writers)
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := s.DB().Conn(ctx)
			if err != nil {
				errCh <- err
				return
			}
			defer conn.Close()
			for i := 0; i < iters; i++ {
				// BEGIN IMMEDIATE 把写锁提前到事务开始时就持有；
				// 期间 sleep 拉长持锁窗口，保证多连接间必然发生写写冲突
				// （修复前即触发 SQLITE_BUSY，修复后由 busy_timeout 等待重试）。
				if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
					errCh <- err
					return
				}
				if _, err := conn.ExecContext(ctx, `INSERT INTO request_log
					(ts, upstream, model, endpoint, status, duration_ms, tokens)
					VALUES (datetime('now'), 'u', 'm', 'chat', 200, 1, 1)`); err != nil {
					conn.ExecContext(ctx, `ROLLBACK`)
					errCh <- err
					return
				}
				time.Sleep(2 * time.Millisecond) // 持锁等待，放大冲突概率
				if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)

	busy := 0
	for err := range errCh {
		if strings.Contains(strings.ToLower(err.Error()), "locked") || strings.Contains(err.Error(), "SQLITE_BUSY") {
			busy++
		}
		t.Errorf("并发写入失败: %v", err)
	}
	if busy > 0 {
		t.Errorf("出现 %d 次 SQLITE_BUSY，busy_timeout 未作用于全部连接", busy)
	}
}