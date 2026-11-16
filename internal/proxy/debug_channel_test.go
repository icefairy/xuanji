package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const debugChatResp = `{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`

// TestDebugChannel_Header 验证：带 X-Xuanji-Debug-Channel 标记头的请求，
// 响应会带上实际命中的上游名 X-Xuanji-Upstream（供 /admin/chat 的 routing 展示）。
func TestDebugChannel_Header(t *testing.T) {
	_, h := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(debugChatResp))
	})

	body := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Xuanji-Debug-Channel", "1")
	rec := httptest.NewRecorder()
	h.ChatCompletions(rec, req)

	if got := rec.Header().Get("X-Xuanji-Upstream"); got != "up" {
		t.Fatalf("X-Xuanji-Upstream = %q, want up（实际命中的上游）", got)
	}
	if got := rec.Header().Get("X-Xuanji-Upstream-Model"); got != "deepseek-ai/DeepSeek-V4-Flash" {
		t.Fatalf("X-Xuanji-Upstream-Model = %q, want deepseek-ai/DeepSeek-V4-Flash", got)
	}
	// 正常响应体不受影响
	if !strings.Contains(rec.Body.String(), `"content":"hi"`) {
		t.Fatalf("body missing content: %s", rec.Body.String())
	}
}

// TestDebugChannel_NoHeader 验证：正常客户端（不带标记头）不会收到上游自定义 header。
func TestDebugChannel_NoHeader(t *testing.T) {
	_, h := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(debugChatResp))
	})

	body := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ChatCompletions(rec, req)

	if got := rec.Header().Get("X-Xuanji-Upstream"); got != "" {
		t.Fatalf("X-Xuanji-Upstream = %q, want empty（正常客户端不应触发）", got)
	}
}
