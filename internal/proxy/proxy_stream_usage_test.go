package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStreamCopy_ExtractsUsage 验证流式透传时从 SSE data 行收集 usage，
// 且透传内容保持完整。
func TestStreamCopy_ExtractsUsage(t *testing.T) {
	h := &Handler{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":20,\"total_tokens\":30}}\n\n" +
		"data: [DONE]\n\n"
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(sse))}
	rec := httptest.NewRecorder()

	var pt, ct int64
	h.streamCopy(rec, resp, &pt, &ct, nil, nil)

	if pt != 10 || ct != 20 {
		t.Errorf("usage = (%d,%d), want (10,20)", pt, ct)
	}
	got := rec.Body.String()
	if !strings.Contains(got, "hi") || !strings.Contains(got, "[DONE]") {
		t.Errorf("passthrough incomplete: %q", got)
	}
}

// TestStreamCopy_NullUsageThenReal 验证上游先发 "usage":null 中间 chunk、
// 最后发真实 usage chunk 时，能正确提取真实值（Exists() 对 null 也 true 的坑）。
func TestStreamCopy_NullUsageThenReal(t *testing.T) {
	h := &Handler{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}],\"usage\":null}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":61}}\n\n" +
		"data: [DONE]\n\n"
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(sse))}
	rec := httptest.NewRecorder()

	var pt, ct int64
	h.streamCopy(rec, resp, &pt, &ct, nil, nil)

	if pt != 8 || ct != 61 {
		t.Errorf("usage = (%d,%d), want (8,61) — null usage chunk must not block real one", pt, ct)
	}
}

// TestStreamCopy_InjectIncludeUsage 验证流式请求未带 stream_options 时，
// 网关主动注入 include_usage=true，上游才能返回 usage chunk。
func TestStreamCopy_InjectIncludeUsage(t *testing.T) {
	var gotBody string
	upstream, h := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	})
	defer upstream.Close()

	doChat(t, h, `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":50}`)

	if !strings.Contains(gotBody, `"stream_options":{"include_usage":true}`) {
		t.Errorf("upstream should receive include_usage=true, got: %s", gotBody)
	}
}

// TestStreamCopy_ForcedIncludeUsage_WithClientStreamOptions 验证客户端已带 stream_options
// （含 include_usage=false 或额外字段）时，网关仍强制注入 include_usage=true 并保留
// 客户端原有 stream_options 字段，上游才能返回 usage chunk 用于计费。
func TestStreamCopy_ForcedIncludeUsage_WithClientStreamOptions(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "client sends empty stream_options",
			body: `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{}}`,
		},
		{
			name: "client sends include_usage=false",
			body: `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":false}}`,
		},
		{
			name: "client sends extra stream_options fields",
			body: `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"unknown_cost":"x"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody string
			upstream, h := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
				buf := make([]byte, r.ContentLength)
				_, _ = r.Body.Read(buf)
				gotBody = string(buf)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
			})
			defer upstream.Close()

			doChat(t, h, tc.body)

			if !strings.Contains(gotBody, `"include_usage":true`) {
				t.Errorf("upstream should receive include_usage=true, got: %s", gotBody)
			}
		})
	}
}

// TestStreamCopy_NoUsageKeepsZero 验证上游不返回 usage chunk 时保持 0（不误报）。
func TestStreamCopy_NoUsageKeepsZero(t *testing.T) {
	h := &Handler{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\ndata: [DONE]\n\n"
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(sse))}
	rec := httptest.NewRecorder()

	var pt, ct int64
	h.streamCopy(rec, resp, &pt, &ct, nil, nil)

	if pt != 0 || ct != 0 {
		t.Errorf("usage = (%d,%d), want (0,0) when upstream omits usage", pt, ct)
	}
}
