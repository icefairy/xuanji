package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/health"
	"github.com/icefairy/xuanji/internal/router"
	"github.com/icefairy/xuanji/internal/store"
)

// TestStreamCopy_ClientInterrupt 验证客户端在流结束前断开（写响应失败）时
// streamCopy 返回 interrupted=true，调用方会把日志状态记为 499。
// 用一个会立即失败的 ResponseWriter（写即错）模拟客户端断连。
type failWriter struct {
	header http.Header
}

func (f *failWriter) Header() http.Header {
	if f.header == nil {
		f.header = make(http.Header)
	}
	return f.header
}
func (f *failWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (f *failWriter) WriteHeader(int)           {}

func TestStreamCopy_ClientInterrupt(t *testing.T) {
	h := &Handler{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":20}}\n\n" +
		"data: [DONE]\n\n"
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(sse))}

	var pt, ct int64
	interrupted, _, _ := h.streamCopy(&failWriter{}, resp, &pt, &ct, nil, nil, time.Now(), nil, nil)

	if !interrupted {
		t.Errorf("interrupted = false, want true（客户端写失败应标记中断）")
	}
}

// TestStreamCopy_NormalFinish 验证流正常透传完毕（[DONE] 收尾）时 interrupted=false。
func TestStreamCopy_NormalFinish(t *testing.T) {
	h := &Handler{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":20}}\n\n" +
		"data: [DONE]\n\n"
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(sse))}
	rec := httptest.NewRecorder()

	var pt, ct int64
	interrupted, _, _ := h.streamCopy(rec, resp, &pt, &ct, nil, nil, time.Now(), nil, nil)

	if interrupted {
		t.Errorf("interrupted = true, want false（正常透传完不应标记中断）")
	}
	if pt != 10 || ct != 20 {
		t.Errorf("usage = (%d,%d), want (10,20)", pt, ct)
	}
}

// cancelStreamTransport 返回一个 200 + SSE 响应，其 body 在发送首个 chunk 后
// 以 context.Canceled 结束读取——模拟流式转发中途客户端断连（请求 context 被取消，
// 上游连接随之中断）。真实场景：客户端关闭连接 → r.Context() 取消 → 上游 body 读失败。
type cancelStreamBody struct{ sent bool }

func (b *cancelStreamBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"), nil
	}
	return 0, context.Canceled
}
func (b *cancelStreamBody) Close() error { return nil }

type cancelStreamTransport struct{}

func (cancelStreamTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &cancelStreamBody{},
	}, nil
}

// TestChatCompletions_StreamClientCancelRecordedAs499 验证流式转发中途客户端断连
// （上游 body 读失败 context.Canceled）时请求日志状态记为 499（client closed request），
// 而不是 502（上游故障）——502 会把客户端断连误算成上游失败、污染可用率统计。
//
// 2026-09-12 日志实测：当天 16 例 `upstream stream read error error="context canceled"`
// 全部被记成 status=502。
func TestChatCompletions_StreamClientCancelRecordedAs499(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rec := store.NewRecorder(s)

	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{Name: "up-cancel", BaseURL: "http://up.invalid/v1", APIKey: "x", Priority: 10, Weight: 100},
		},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules:           []config.Rule{{Model: "m", Upstreams: []string{"up-cancel"}, Strategy: "primary_backup"}},
		},
	}
	h := New(cfg, router.New(cfg), health.New(cfg))
	h.SetRecorder(rec)
	h.client = &http.Client{Transport: cancelStreamTransport{}}

	doChat(t, h, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	rec.Close() // flush

	rows, err := s.DB().Query(`SELECT status FROM request_log`)
	if err != nil {
		t.Fatalf("query request_log: %v", err)
	}
	defer rows.Close()
	var statuses []int
	for rows.Next() {
		var st int
		if err := rows.Scan(&st); err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, st)
	}
	if len(statuses) == 0 {
		t.Fatal("no request_log record")
	}
	for _, st := range statuses {
		if st == http.StatusBadGateway {
			t.Errorf("status = 502, want 499（客户端断连不应记为上游故障）; all=%v", statuses)
		}
	}
	found := false
	for _, st := range statuses {
		if st == 499 {
			found = true
		}
	}
	if !found {
		t.Errorf("no status=499 record, want client-close semantics; all=%v", statuses)
	}
}
