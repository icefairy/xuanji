// Package debuglog 实现「异常请求完整诊断日志」的落盘（NDJSON 单文件 + 轮转）。
//
// 设计取向（以简驭繁）：
//   - 只做一件事：把调用方给的结构体序列化成一行 JSON 追加写文件；
//   - 路径/权限/轮转/保留策略全部固化在本包，不通过 HTTP 暴露（无读取接口）；
//   - 并发安全（单个 Writer 多 goroutine 共享），写失败只降级为一次性告警，不影响转发。
//
// 与 request_log 的分工：request_log 是**长期**结构化指标（不含请求体）；
// 本包是**短时排障**用的完整报文快照（含完整提示词），默认关闭、保留 7 天即删。
package debuglog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// 默认策略：单文件 100MB 轮转，保留 7 天。
const (
	DefaultMaxFileBytes int64 = 100 << 20
	DefaultRetainDays         = 7
	dirPerm                   = 0o700
	filePerm                  = 0o600
	fileNamePrefix            = "debug-"
	fileNameExt               = ".ndjson"
)

// Writer 是并发安全的 NDJSON 追加写入器。
//
// 生命周期：New → 多次 Write → Close（进程退出或开关关闭）。
// Close 之后再 Write 返回错误但不 panic（避免关闭竞态把请求panic穿到客户端）。
type Writer struct {
	mu            sync.Mutex
	dir           string
	maxFileBytes  int64
	retainDays    int
	now           func() time.Time // 可注入，测试轮转/清理用
	file          *os.File
	fileBytes     int64
	fileDate      string // 当前文件日期 yyyyMMdd，跨天换新文件
	warnedWrite   atomic.Bool
	closed        bool
	RotationCount int64 // 轮转次数（测试断言用；单调递增）
}

// Options 用于覆盖默认策略（0 值表示用默认）。
type Options struct {
	MaxFileBytes int64
	RetainDays   int
	// Now 注入时钟（测试用）；nil 用 time.Now。
	Now func() time.Time
}

// New 创建/打开目录下的 writer。
// 目录不存在时以 0700 创建；已存在时**不改动**已有权限（避免覆盖运维设置）。
func New(dir string, opt Options) (*Writer, error) {
	if dir == "" {
		return nil, fmt.Errorf("debuglog: empty dir")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("debuglog: abs dir: %w", err)
	}
	if err := os.MkdirAll(abs, dirPerm); err != nil {
		return nil, fmt.Errorf("debuglog: mkdir %s: %w", abs, err)
	}
	w := &Writer{
		dir:          abs,
		maxFileBytes: opt.MaxFileBytes,
		retainDays:   opt.RetainDays,
		now:          opt.Now,
	}
	if w.maxFileBytes <= 0 {
		w.maxFileBytes = DefaultMaxFileBytes
	}
	if w.retainDays <= 0 {
		w.retainDays = DefaultRetainDays
	}
	if w.now == nil {
		w.now = time.Now
	}
	// 启动即清理过期文件：避免长期停机后重新开启时把老日志继续留着。
	w.mu.Lock()
	_ = w.purgeLocked()
	w.mu.Unlock()
	return w, nil
}

// Dir 返回日志目录绝对路径。
func (w *Writer) Dir() string { return w.dir }

// Path 返回当前日志文件路径（尚未创建时返回按今天推算的路径）。
func (w *Writer) Path() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		return w.file.Name()
	}
	return w.filePathLocked(w.now())
}

// Write 把一条记录序列化为一行 JSON 追加写入。
// 记录内不得包含请求头/凭证——调用方负责（本包不做字段过滤，避免静默改写数据）。
func (w *Writer) Write(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("debuglog: marshal: %w", err)
	}
	data = append(data, '\n')

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return fmt.Errorf("debuglog: writer closed")
	}
	if err := w.openLocked(); err != nil {
		return err
	}
	// 超限判断放在写入前：一行可能很大（完整提示词），
	// 允许单文件略微超过上限，但保证不会在同一天无限增长。
	if w.fileBytes > 0 && w.fileBytes+int64(len(data)) > w.maxFileBytes {
		if err := w.rotateLocked(); err != nil {
			return err
		}
	}
	n, werr := w.file.Write(data)
	w.fileBytes += int64(n)
	if werr != nil {
		return fmt.Errorf("debuglog: write: %w", werr)
	}
	return nil
}

// Sync 刷盘（关闭前与测试用）。
func (w *Writer) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	return w.file.Sync()
}

// Close 刷盘并关闭文件句柄；可重复调用。
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if w.file == nil {
		return nil
	}
	ferr := w.file.Sync()
	cerr := w.file.Close()
	w.file = nil
	if ferr != nil {
		return ferr
	}
	return cerr
}

// WarnOnce 需要向外部告警（如 slog）时的节流标志：
// 返回 true 表示这是第一次，调用方据此只打印一次，避免磁盘满时刷屏。
func (w *Writer) WarnOnce() bool { return w.warnedWrite.CompareAndSwap(false, true) }

// ===== 内部实现 =====

func (w *Writer) dateStr(t time.Time) string { return t.Format("20060102") }

func (w *Writer) filePathLocked(t time.Time) string {
	return filepath.Join(w.dir, fileNamePrefix+w.dateStr(t)+fileNameExt)
}

// openLocked 按需打开/切换当前文件（跨天换新文件 + 清理过期）。
func (w *Writer) openLocked() error {
	today := w.dateStr(w.now())
	if w.file != nil && w.fileDate == today {
		return nil
	}
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
	path := w.filePathLocked(w.now())
	// O_APPEND：多进程/重启后接着写同一文件，不覆盖。
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		return fmt.Errorf("debuglog: open %s: %w", path, err)
	}
	// 已存在文件可能由旧版本/运维创建为更宽松的权限：显式收紧到 0600。
	_ = f.Chmod(filePerm)
	st, serr := f.Stat()
	if serr == nil {
		w.fileBytes = st.Size()
	} else {
		w.fileBytes = 0
	}
	w.file = f
	w.fileDate = today
	_ = w.purgeLocked()
	return nil
}

// rotateLocked 把当天文件改名成带序号的历史文件后新开一个空文件。
// 命名：debug-YYYYMMDD.1.ndjson → .2 → ...（序号递增，天然按时间排序）。
func (w *Writer) rotateLocked() error {
	if w.file != nil {
		_ = w.file.Sync()
		_ = w.file.Close()
		w.file = nil
	}
	base := w.filePathLocked(w.now())
	stem := fileNamePrefix + w.fileDate
	for i := 1; i < 10000; i++ {
		dst := filepath.Join(w.dir, fmt.Sprintf("%s.%d%s", stem, i, fileNameExt))
		if _, err := os.Stat(dst); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("debuglog: rotate stat %s: %w", dst, err)
		}
		if err := os.Rename(base, dst); err != nil {
			return fmt.Errorf("debuglog: rotate rename: %w", err)
		}
		break
	}
	w.RotationCount++
	w.fileBytes = 0
	return w.openLocked()
}

// purgeLocked 删除 mtime 早于保留期的 debug-*.ndjson 文件（含轮转历史）。
// 只删本包命名规则内的文件，不动目录里用户自己的东西。
func (w *Writer) purgeLocked() error {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return err
	}
	cutoff := w.now().Add(-time.Duration(w.retainDays) * 24 * time.Hour)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !matchLogFile(name) {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(w.dir, name))
		}
	}
	return nil
}

// matchLogFile 判断文件名是否属于本包的日志文件：
// debug-YYYYMMDD.ndjson 或 debug-YYYYMMDD.<n>.ndjson。
func matchLogFile(name string) bool {
	const stemLen = len(fileNamePrefix) + 8 // "debug-" + yyyyMMdd
	if len(name) < stemLen+len(fileNameExt) {
		return false
	}
	if name[:len(fileNamePrefix)] != fileNamePrefix {
		return false
	}
	for i := len(fileNamePrefix); i < stemLen; i++ {
		if name[i] < '0' || name[i] > '9' {
			return false
		}
	}
	rest := name[stemLen:]
	if rest == fileNameExt {
		return true
	}
	if len(rest) <= len(fileNameExt) || rest[len(rest)-len(fileNameExt):] != fileNameExt {
		return false
	}
	num := rest[1 : len(rest)-len(fileNameExt)]
	if num == "" {
		return false
	}
	for i := 0; i < len(num); i++ {
		if num[i] < '0' || num[i] > '9' {
			return false
		}
	}
	return true
}
