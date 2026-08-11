package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	interrupted := h.streamCopy(&failWriter{}, resp, &pt, &ct, nil, nil)

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
	interrupted := h.streamCopy(rec, resp, &pt, &ct, nil, nil)

	if interrupted {
		t.Errorf("interrupted = true, want false（正常透传完不应标记中断）")
	}
	if pt != 10 || ct != 20 {
		t.Errorf("usage = (%d,%d), want (10,20)", pt, ct)
	}
}
