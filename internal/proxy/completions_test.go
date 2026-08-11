package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/health"
	"github.com/icefairy/xuanji/internal/router"
	"github.com/icefairy/xuanji/internal/store"
)

// completionsTestConfig 构造 /v1/completions 测试用的最小配置（单上游）。
func completionsTestConfig(srvURL string) *config.Config {
	return &config.Config{
		Upstreams: []config.Upstream{
			{
				Name: "test-up", BaseURL: srvURL + "/v1", APIKey: "sk-test",
				Priority: 1, Weight: 100,
				Models: []string{"deepseek-v4-flash"},
			},
		},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules: []config.Rule{
				{Model: "deepseek-v4-flash", Upstreams: []string{"test-up"}, Strategy: "primary_backup"},
			},
		},
		Retry: config.Retry{MaxRetries: 3, RetryStatuses: []int{429, 500, 502, 503, 504}},
	}
}

// TestCompletions_PromptString 验证 prompt 为 string 时：
// 上游收到 chat 格式请求（messages 含一条 user 消息），响应转回 completions 格式。
func TestCompletions_PromptString(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path = %q, want /v1/chat/completions", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		if json.Unmarshal(b, &gotBody) != nil {
			t.Errorf("upstream body not json: %s", b)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"chatcmpl-test","object":"chat.completion","created":1750000000,"model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"你好，世界！"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}`)
	}))
	defer srv.Close()

	h := New(completionsTestConfig(srv.URL), router.New(completionsTestConfig(srv.URL)), health.New(completionsTestConfig(srv.URL)))

	req := httptest.NewRequest(http.MethodPost, "/v1/completions", strings.NewReader(`{"model":"deepseek-v4-flash","prompt":"hello","max_tokens":100,"temperature":0.7}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Completions(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	// 上游收到的应是 chat 格式：model 不变 + messages 单条 user
	if gotBody["model"] != "deepseek-v4-flash" {
		t.Errorf("upstream model = %v, want deepseek-v4-flash", gotBody["model"])
	}
	msgs, ok := gotBody["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("upstream messages = %v, want 1 user message", gotBody["messages"])
	}
	msg := msgs[0].(map[string]any)
	if msg["role"] != "user" || msg["content"] != "hello" {
		t.Errorf("upstream message = %v, want {role:user content:hello}", msg)
	}
	// 采样参数原样搬运
	if gotBody["max_tokens"] != float64(100) || gotBody["temperature"] != 0.7 {
		t.Errorf("sampling params not carried: %v", gotBody)
	}
	// 响应为 completions 格式
	var out struct {
		Object  string `json:"object"`
		Choices []struct {
			Text         string `json:"text"`
			Index        int    `json:"index"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response not json: %s", rec.Body.String())
	}
	if out.Object != "text_completion" {
		t.Errorf("object = %q, want text_completion", out.Object)
	}
	if len(out.Choices) != 1 || out.Choices[0].Text == "" {
		t.Errorf("choices = %+v, want 1 choice with non-empty text", out.Choices)
	}
	if out.Choices[0].Index != 0 || out.Choices[0].FinishReason != "stop" {
		t.Errorf("choice = %+v, want index 0 finish_reason stop", out.Choices[0])
	}
	if out.Usage.PromptTokens != 5 || out.Usage.CompletionTokens != 7 {
		t.Errorf("usage = %+v, want prompt 5 completion 7", out.Usage)
	}
}

// TestCompletions_PromptArray 验证 prompt 为数组时按行拼合为一条 user 消息。
func TestCompletions_PromptArray(t *testing.T) {
	var gotMessages string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotMessages = string(b)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	h := New(completionsTestConfig(srv.URL), router.New(completionsTestConfig(srv.URL)), health.New(completionsTestConfig(srv.URL)))

	req := httptest.NewRequest(http.MethodPost, "/v1/completions", strings.NewReader(`{"model":"deepseek-v4-flash","prompt":["line1","line2","line3"]}`))
	rec := httptest.NewRecorder()
	h.Completions(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(gotMessages, `"content":"line1\nline2\nline3"`) {
		t.Errorf("upstream body = %s, want content joined with \\n", gotMessages)
	}
}

// TestCompletions_StreamNotSupported 验证 stream=true 返回明确 400。
func TestCompletions_StreamNotSupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"x","choices":[]}`)
	}))
	defer srv.Close()

	h := New(completionsTestConfig(srv.URL), router.New(completionsTestConfig(srv.URL)), health.New(completionsTestConfig(srv.URL)))

	req := httptest.NewRequest(http.MethodPost, "/v1/completions", strings.NewReader(`{"model":"deepseek-v4-flash","prompt":"hi","stream":true}`))
	rec := httptest.NewRecorder()
	h.Completions(rec, req)

	if rec.Code != 400 {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "stream not supported for /v1/completions") {
		t.Errorf("body = %s, want clear stream-not-supported message", rec.Body.String())
	}
}

// TestCompletions_MissingPrompt 验证缺 prompt 返回 400。
func TestCompletions_MissingPrompt(t *testing.T) {
	h := New(&config.Config{Retry: config.Retry{MaxRetries: 1, RetryStatuses: []int{}}}, router.New(&config.Config{}), health.New(&config.Config{}))
	req := httptest.NewRequest(http.MethodPost, "/v1/completions", strings.NewReader(`{"model":"m"}`))
	rec := httptest.NewRecorder()
	h.Completions(rec, req)
	if rec.Code != 400 {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "prompt is required") {
		t.Errorf("body = %s, want prompt is required", rec.Body.String())
	}
}

// TestCompletions_LogEndpoint 验证请求日志 endpoint 记为 "completions"（与 chat 区分）。
func TestCompletions_LogEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`)
	}))
	defer srv.Close()

	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rec := store.NewRecorder(s)

	h := New(completionsTestConfig(srv.URL), router.New(completionsTestConfig(srv.URL)), health.New(completionsTestConfig(srv.URL)))
	h.SetRecorder(rec)

	req := httptest.NewRequest(http.MethodPost, "/v1/completions", strings.NewReader(`{"model":"deepseek-v4-flash","prompt":"hi"}`))
	req.RemoteAddr = "10.0.0.9:12345"
	req.Header.Set("User-Agent", "legacy-prog/1.0")
	recw := httptest.NewRecorder()
	h.Completions(recw, req)
	rec.Close() // flush 落库

	if recw.Code != 200 {
		t.Fatalf("status=%d body=%s", recw.Code, recw.Body.String())
	}
	rows, err := s.DB().Query(`SELECT endpoint, model, user_agent, client_addr, prompt_tokens, completion_tokens FROM request_log`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var endpoint, model, ua, addr string
		var pt, ct int64
		if rows.Scan(&endpoint, &model, &ua, &addr, &pt, &ct) != nil {
			continue
		}
		if endpoint == "completions" {
			found = true
			if model != "deepseek-v4-flash" || ua != "legacy-prog/1.0" || addr != "10.0.0.9:12345" {
				t.Errorf("record = endpoint=%s model=%s ua=%s addr=%s, want completions/deepseek-v4-flash/legacy-prog/1.0/10.0.0.9:12345", endpoint, model, ua, addr)
			}
			if pt != 2 || ct != 3 {
				t.Errorf("record tokens = %d/%d, want 2/3", pt, ct)
			}
		}
	}
	if !found {
		t.Errorf("no request_log record with endpoint=completions")
	}
}
