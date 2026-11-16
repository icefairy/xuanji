package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/router"
	"github.com/icefairy/xuanji/internal/store"
)

// 本文件覆盖「流式中断后，chat/completions 日志里的 status 必须与 request_log 一致」。
//
// 背景（2026-10-23 实测）：DB 里当天 4 条非 200 全来自流中断/读错——
//   - 3 条 499：`upstream stream read error: context canceled`
//   - 1 条 502：`upstream stream read error: ... connection reset by peer`
//
// 而 supervisor 日志里这 4 条对应行**全部是 status=200**：
//
//	21:22:19 WARN upstream stream read error error="read tcp ...: connection reset by peer"
//	21:22:19 WARN codebuddy 流转发中断 upstream=workbuddy error="..."
//	21:22:19 INFO chat/completions ... upstream=workbuddy status=200 duration=1m43.9s
//
// 根因：归一化（499/502）只发生在 forwardOnce 的落库 defer 里，写的是局部变量 status；
// ChatCompletions 的日志 defer 读的是 rec.status（真实写出的 HTTP 码 = 200，因为流式
// 响应头早就发出去了）。两条记录口径不一致 → 日志「全部 200」、页面/DB 却有失败。
// 2026-10-21 那次修复只处理了 status==0（源头即断连）的情形，漏了这种「已发 200 头」的。

// logRecorder 捕获 Handler 的 slog 输出，供断言日志字段。
type logRecorder struct {
	buf bytes.Buffer
}

func (l *logRecorder) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(&l.buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// findChatStatus 从捕获的日志里取 msg=="chat/completions" 的 status 字段。
// 返回 (status, found)。
func (l *logRecorder) findChatStatus(t *testing.T) (int, bool) {
	t.Helper()
	for _, line := range strings.Split(l.buf.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue // 非 JSON 行（不应出现，但容忍）
		}
		if m["msg"] != "chat/completions" {
			continue
		}
		s, ok := m["status"].(float64)
		if !ok {
			return 0, false
		}
		return int(s), true
	}
	return 0, false
}

// newStreamInterruptHandler 造一个「上游已回 200 + 流中途读错」的场景。
// readErr 决定流读取以什么错误结束：
//   - context.Canceled → 期望日志与 DB 都是 499
//   - 其他（如 connection reset）→ 期望日志与 DB 都是 502
// vendorStreamBody 按脚本产出数据，最后以指定错误结束读取。
type vendorStreamBody struct {
	chunks []string
	i      int
	err    error
	// delay 在**首个 chunk 之前**阻塞一段时间：用于让 TTFT 明显 > 0
	// （内存读取耗时 < 1ms，被毫秒截断后测不出）。
	delay  time.Duration
	waited bool
}

func (b *vendorStreamBody) Read(p []byte) (int, error) {
	// 首个 chunk 前延迟一次（TTFT 断言用）
	if b.delay > 0 && !b.waited {
		b.waited = true
		time.Sleep(b.delay)
	}
	if b.i < len(b.chunks) {
		n := copy(p, b.chunks[b.i])
		b.i++
		return n, nil
	}
	return 0, b.err
}
func (b *vendorStreamBody) Close() error { return nil }

func newStreamInterruptHandler(t *testing.T, readErr error) (*Handler, *store.Store, *logRecorder) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "xuanji.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	body := &vendorStreamBody{
		chunks: []string{`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n"},
		err:    readErr,
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// 先吐一块，再以错误结束——模拟「已发 200 头，流被截断」
		_, _ = w.Write([]byte(body.chunks[0]))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// 制造读取错误：直接关闭连接
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, herr := hj.Hijack()
			if herr == nil {
				_ = conn.Close()
			}
		}
	}))
	t.Cleanup(up.Close)

	cfg := &config.Config{
		Upstreams: []config.Upstream{{
			Name: "upstream-a", Type: "openai",
			BaseURL: up.URL, APIKey: "sk-test", Priority: 10, Weight: 100,
			Models: []string{"m-1"},
		}},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules: []config.Rule{
				{Model: "m-1", Upstreams: []string{"upstream-a"}, Strategy: "primary_backup"},
			},
		},
		Retry: config.Retry{MaxRetries: 0},
	}
	h := New(cfg, router.New(cfg), nil)
	h.SetStore(st)
	h.SetRecorder(store.NewRecorder(st))
	lr := &logRecorder{}
	h.log = lr.logger()
	return h, st, lr
}

// TestChatCompletions_LogStatusMatchesDBOnStreamReadError 流被读错截断时，
// 日志 status 必须与 DB 一致（502），不能记 200。
func TestChatCompletions_LogStatusMatchesDBOnStreamReadError(t *testing.T) {
	h, st, lr := newStreamInterruptHandler(t, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m-1","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.ChatCompletions(rec, req)
	h.recorder.Close()

	logStatus, ok := lr.findChatStatus(t)
	if !ok {
		t.Fatal("未捕获到 chat/completions 日志行")
	}
	dbStatus := querySingleStatus(t, st)
	if dbStatus != http.StatusBadGateway {
		t.Fatalf("前置条件失败：DB status = %d, want 502（流被截断应记上游故障）", dbStatus)
	}
	if logStatus != dbStatus {
		t.Fatalf("日志 status = %d 与 DB status = %d 不一致；"+
			"2026-10-23 实测：DB 有 4 条非 200（3×499 + 1×502），日志却全是 200",
			logStatus, dbStatus)
	}
}

// TestChatCompletions_LogStatusIs499OnClientCancelDuringStream 客户端在流式
// 传输途中断连时，日志与 DB 都必须记 499。
func TestChatCompletions_LogStatusIs499OnClientCancelDuringStream(t *testing.T) {
	h, st, lr := newStreamInterruptHandler(t, context.Canceled)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 预先取消：让流读取立刻拿到 context.Canceled
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m-1","messages":[{"role":"user","content":"hi"}],"stream":true}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.ChatCompletions(rec, req)
	h.recorder.Close()

	logStatus, ok := lr.findChatStatus(t)
	if !ok {
		t.Fatal("未捕获到 chat/completions 日志行")
	}
	// 该路径下请求一开始就取消，可能一条记录都不落（源头断连走别的分支），
	// 此时只要求日志不是 200。
	rows := queryStatuses(t, st)
	if len(rows) == 0 {
		if logStatus == http.StatusOK {
			t.Fatalf("客户端断连时日志 status = 200；应为 499（client closed request）")
		}
		return
	}
	if logStatus == http.StatusOK {
		t.Fatalf("客户端断连时日志 status = 200，但 DB 有记录 %v；口径不一致", rows)
	}
	if logStatus != clientClosedRequest {
		t.Fatalf("日志 status = %d, want 499；DB=%v", logStatus, rows)
	}
}

// querySingleStatus 返回 request_log 唯一的 status。
func querySingleStatus(t *testing.T, st *store.Store) int {
	t.Helper()
	rows := queryStatuses(t, st)
	if len(rows) != 1 {
		t.Fatalf("request_log 期望 1 条记录，got %d: %v", len(rows), rows)
	}
	return rows[0]
}

// queryStatuses 返回 request_log 全部 status。
func queryStatuses(t *testing.T, st *store.Store) []int {
	t.Helper()
	rs, err := st.DB().Query(`SELECT status FROM request_log ORDER BY id`)
	if err != nil {
		t.Fatalf("query request_log: %v", err)
	}
	defer rs.Close()
	var out []int
	for rs.Next() {
		var s int
		if err := rs.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// TestChatCompletions_LogStatusStays200OnSuccess 反向保护：正常完成的流式请求
// 日志仍记 200，修复不得把成功请求误记成 499/502。
func TestChatCompletions_LogStatusStays200OnSuccess(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "xuanji.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	cfg := &config.Config{
		Upstreams: []config.Upstream{{
			Name: "upstream-a", Type: "openai", BaseURL: srv.URL,
			APIKey: "sk-test", Priority: 10, Weight: 100, Models: []string{"m-1"},
		}},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules:           []config.Rule{{Model: "m-1", Upstreams: []string{"upstream-a"}, Strategy: "primary_backup"}},
		},
		Retry: config.Retry{MaxRetries: 0},
	}
	h := New(cfg, router.New(cfg), nil)
	h.SetStore(st)
	h.SetRecorder(store.NewRecorder(st))
	lr := &logRecorder{}
	h.log = lr.logger()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m-1","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ChatCompletions(rec, req)
	h.recorder.Close()

	logStatus, ok := lr.findChatStatus(t)
	if !ok {
		t.Fatal("未捕获到 chat/completions 日志行")
	}
	if logStatus != http.StatusOK {
		t.Fatalf("成功请求日志 status = %d, want 200（修复不得把成功误记失败）", logStatus)
	}
	if got := querySingleStatus(t, st); got != http.StatusOK {
		t.Fatalf("DB status = %d, want 200", got)
	}
}
