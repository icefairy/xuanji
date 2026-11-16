package debuglog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	s := strings.TrimRight(string(data), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func TestWriter_Permissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "debug-logs")
	w, err := New(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.Write(map[string]string{"a": "1"}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir perm = %o, want 700", perm)
	}
	fst, err := os.Stat(w.Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := fst.Mode().Perm(); perm != 0o600 {
		t.Errorf("file perm = %o, want 600", perm)
	}
}

func TestWriter_RoundTripRawJSON(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// 超长 + 中文 + 引号 + 换行 + 制表符
	long := strings.Repeat("长文本\"引号\"\n换行\t", 5000)
	rec := map[string]any{"raw_request": json.RawMessage(``)}
	rec["raw_request"] = json.RawMessage(`{"model":"m","messages":[{"role":"user","content":` + mustJSON(t, long) + `}]}`)
	if err := w.Write(rec); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	lines := readLines(t, w.Path())
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(lines))
	}
	var got struct {
		RawRequest struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		} `json:"raw_request"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("unmarshal line: %v", err)
	}
	if got.RawRequest.Model != "m" {
		t.Errorf("model = %q", got.RawRequest.Model)
	}
	if got.RawRequest.Messages[0].Content != long {
		t.Errorf("长文本未原样 round-trip（len=%d want %d）",
			len(got.RawRequest.Messages[0].Content), len(long))
	}
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWriter_ConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := w.Write(map[string]int{"i": i}); err != nil {
				t.Errorf("write: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, w.Path())
	if len(lines) != n {
		t.Fatalf("lines = %d, want %d（并发写丢行或串行）", len(lines), n)
	}
	for _, ln := range lines {
		var v map[string]int
		if err := json.Unmarshal([]byte(ln), &v); err != nil {
			t.Fatalf("并发写产生残缺行: %q (%v)", ln, err)
		}
	}
}

func TestWriter_Rotation(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.Local)
	w, err := New(dir, Options{MaxFileBytes: 200, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	payload := strings.Repeat("x", 100)
	for i := 0; i < 10; i++ {
		if err := w.Write(map[string]string{"p": payload}); err != nil {
			t.Fatal(err)
		}
	}
	if w.RotationCount == 0 {
		t.Fatal("超过 200B 未轮转")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("轮转后应有多份文件, got %d", len(entries))
	}
	// 文件体积不得（大幅）超过上限：单行允许略微超出，但轮转必须发生在写入前
	for _, e := range entries {
		info, _ := e.Info()
		if info.Size() > 400 {
			t.Errorf("%s size = %d, 轮转失效", e.Name(), info.Size())
		}
		// 轮转出来的文件必须仍是 0600
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s perm = %o, want 600", e.Name(), perm)
		}
	}
}

func TestWriter_DailyFileSwitch(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 9, 23, 0, 0, 0, time.Local)
	w, err := New(dir, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.Write(map[string]string{"d": "1"}); err != nil {
		t.Fatal(err)
	}
	first := w.Path()
	now = now.Add(2 * time.Hour) // 跨天
	if err := w.Write(map[string]string{"d": "2"}); err != nil {
		t.Fatal(err)
	}
	if w.Path() == first {
		t.Fatalf("跨天未换新文件: %s", first)
	}
	if len(readLines(t, first)) != 1 || len(readLines(t, w.Path())) != 1 {
		t.Error("跨天写入未分离到各自文件")
	}
}

func TestWriter_Retention(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.Local)
	// 预置：8 天前、6 天前各一个日志文件，以及一个无关文件
	old := filepath.Join(dir, "debug-20261101.ndjson")
	fresh := filepath.Join(dir, "debug-20261103.ndjson")
	other := filepath.Join(dir, "notes.txt")
	for _, p := range []string{old, fresh, other} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ts := time.Date(2026, 11, 1, 12, 0, 0, 0, time.Local)
	if err := os.Chtimes(old, ts, ts); err != nil {
		t.Fatal(err)
	}
	ts = time.Date(2026, 11, 3, 12, 0, 0, 0, time.Local)
	if err := os.Chtimes(fresh, ts, ts); err != nil {
		t.Fatal(err)
	}

	w, err := New(dir, Options{RetainDays: 7, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("超过 7 天的日志文件未被删除")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("保留期内的日志文件被误删")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("非本包命名的文件被误删")
	}
}

func TestWriter_CloseIdempotentAndRejectsWrite(t *testing.T) {
	w, err := New(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(map[string]string{"a": "1"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("重复 Close 应无错: %v", err)
	}
	if err := w.Write(map[string]string{"a": "2"}); err == nil {
		t.Error("Close 后 Write 应返回错误")
	}
	// 关闭后内容已 flush 到磁盘
	if lines := readLines(t, w.Path()); len(lines) != 1 {
		t.Errorf("Close 后文件行数 = %d, want 1", len(lines))
	}
}

func TestMatchLogFile(t *testing.T) {
	cases := map[string]bool{
		"debug-20261109.ndjson":     true,
		"debug-20261109.1.ndjson":   true,
		"debug-20261109.42.ndjson":  true,
		"debug-2026110.ndjson":      false,
		"debug-20261109.json":       false,
		"xdebug-20261109.ndjson":    false,
		"debug-20261109.x.ndjson":   false,
		"debug-20261109.ndjson.bak": false,
	}
	for name, want := range cases {
		if got := matchLogFile(name); got != want {
			t.Errorf("matchLogFile(%q) = %v, want %v", name, got, want)
		}
	}
}
