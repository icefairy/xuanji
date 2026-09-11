package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/icefairy/xuanji/internal/health"
)

// TestStreamCopy_LineTooLong 验证 SSE 单行超过 scanner 缓冲（1MB）时，streamCopy
// 必须返回 readErr（而非静默当作正常 EOF 截断流）。此前缺失 scanner.Err() 检查，
// 会向客户端透传残缺的 200 流且计费/统计按成功处理。
func TestStreamCopy_LineTooLong(t *testing.T) {
	h := &Handler{log: slog.New(slog.NewTextHandler(io.Discard, nil)), health: &health.Checker{}}
	big := make([]byte, 2<<20)
	for i := range big {
		big[i] = 'a'
	}
	// 单行超 1MB 的 data 行（无换行），紧跟一个合法的 [DONE] 行。
	sse := "data: {" + string(big) + "}\n" + "data: [DONE]\n\n"
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(sse))}
	rec := httptest.NewRecorder()

	var pt, ct int64
	_, _, readErr := h.streamCopy(rec, resp, &pt, &ct, nil, nil, time.Now(), nil, nil)
	if readErr == nil {
		t.Fatal("want readErr != nil for oversized SSE line, got nil")
	}
}

// TestShouldMarkUpstreamFailure 验证 media/video 的失败判定与 chat 主循环一致：
// 客户端断连与 429 限流不计为上游故障（避免误拉黑健康上游）。
func TestShouldMarkUpstreamFailure(t *testing.T) {
	h := &Handler{log: slog.New(slog.NewTextHandler(io.Discard, nil)), health: &health.Checker{}}
	// 已写出响应（handled）：不应再记失败
	if h.shouldMarkUpstreamFailure(true, errors.New("boom")) {
		t.Error("handled=true should not mark failure")
	}
	// 429 限流：不计失败
	if h.shouldMarkUpstreamFailure(false, errors.New("upstream error: 429 Too Many Requests")) {
		t.Error("429 should not mark failure")
	}
	// 客户端断连：不计失败
	if h.shouldMarkUpstreamFailure(false, context.Canceled) {
		t.Error("client canceled should not mark failure")
	}
	// 真正的连接错误：应记失败
	if !h.shouldMarkUpstreamFailure(false, errors.New("dial tcp: connection refused")) {
		t.Error("connection error should mark failure")
	}
}
