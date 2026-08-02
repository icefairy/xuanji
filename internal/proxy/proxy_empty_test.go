package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/router"
)

// TestIsEmptyCompletion 验证"空内容完成"判定：
// content 空 + finish_reason=length 且**无任何思考字段**才判为空（应切换候选）；
// 思考字段（reasoning / reasoning_content）有值 = 模型正常在思考（max_tokens 短未输出正文），判为正常；
// tool_calls / 正常内容 / 空 choices 等合法场景不得误伤。
func TestIsEmptyCompletion(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "正常内容",
			body: `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`,
			want: false,
		},
		{
			name: "真·空内容：content空+length+无思考字段",
			body: `{"choices":[{"message":{"role":"assistant","content":null},"finish_reason":"length"}],"usage":{"completion_tokens":100}}`,
			want: true,
		},
		{
			name: "思考中：content空+length但reasoning有值（商汤风格）",
			body: `{"choices":[{"message":{"role":"assistant","content":null,"reasoning":"思考中..."},"finish_reason":"length"}],"usage":{"completion_tokens":100}}`,
			want: false,
		},
		{
			name: "思考中：content空+length但reasoning_content有值（DeepSeek风格）",
			body: `{"choices":[{"message":{"role":"assistant","content":null,"reasoning_content":"让我一步步思考..."},"finish_reason":"length"}],"usage":{"completion_tokens":100}}`,
			want: false,
		},
		{
			name: "函数调用：content空但tool_calls",
			body: `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"function":{"name":"f"}}]},"finish_reason":"tool_calls"}]}`,
			want: false,
		},
		{
			name: "choices空数组",
			body: `{"id":"x","object":"chat.completion","choices":[]}`,
			want: false,
		},
		{
			name: "content空但finish_reason=stop（模型主动不输出）",
			body: `{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`,
			want: false,
		},
		{
			name: "content有值即使length截断",
			body: `{"choices":[{"message":{"role":"assistant","content":"半句话"},"finish_reason":"length"}]}`,
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsEmptyCompletion([]byte(c.body)); got != c.want {
				t.Errorf("IsEmptyCompletion(%s) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

// TestChatCompletions_EmptyCompletionFailover 验证：上游1返回"200但content空"（无思考字段），
// 网关应判定为可重试并切换候选，客户端收到上游2的正常内容。
func TestChatCompletions_EmptyCompletionFailover(t *testing.T) {
	var badCalled, goodCalled int
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		badCalled++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// 真·空响应：content null + 无思考字段 + finish_reason=length + usage
		w.Write([]byte(`{"id":"bad","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":null},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":100,"total_tokens":110}}`))
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		goodCalled++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"good","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"正常回复"},"finish_reason":"stop"}]}`))
	}))
	defer good.Close()

	cfg := &config.Config{
		Retry: config.Retry{MaxRetries: 3, RetryStatuses: []int{429, 500}},
		Upstreams: []config.Upstream{
			{Name: "bad-up", BaseURL: bad.URL, APIKey: "k", Priority: 1, Models: []string{"deepseek-v4-flash"}, ModelMapping: map[string]string{"deepseek-v4-flash": "deepseek-v4-flash"}},
			{Name: "good-up", BaseURL: good.URL, APIKey: "k", Priority: 2, Models: []string{"deepseek-v4-flash"}, ModelMapping: map[string]string{"deepseek-v4-flash": "deepseek-v4-flash"}},
		},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules:           []config.Rule{{Model: "deepseek-v4-flash", Upstreams: []string{"bad-up", "good-up"}, Strategy: "primary_backup"}},
		},
	}
	h := New(cfg, router.New(cfg), nil)

	rec := doChat(t, h, `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hello"}]}`)
	if badCalled != 1 {
		t.Errorf("bad upstream called %d times, want 1 (只应尝试一次)", badCalled)
	}
	if goodCalled != 1 {
		t.Errorf("good upstream called %d times, want 1", goodCalled)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "正常回复") {
		t.Errorf("client should receive good upstream content, got: %s", rec.Body.String())
	}
}

// TestChatCompletions_ThinkingNoFailover 验证新语义：上游返回"200但content空+reasoning有值"
// （思考型模型 max_tokens 短，正在思考），应判为**正常响应直接透传**，不切换候选。
func TestChatCompletions_ThinkingNoFailover(t *testing.T) {
	var badCalled, goodCalled int
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		badCalled++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// 商汤风格：content null + reasoning 有值 + finish_reason=length
		w.Write([]byte(`{"id":"bad","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":null,"reasoning":"思考中..."},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":100,"total_tokens":110}}`))
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		goodCalled++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"good","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"正常回复"},"finish_reason":"stop"}]}`))
	}))
	defer good.Close()

	cfg := &config.Config{
		Retry: config.Retry{MaxRetries: 3, RetryStatuses: []int{429, 500}},
		Upstreams: []config.Upstream{
			{Name: "bad-up", BaseURL: bad.URL, APIKey: "k", Priority: 1, Models: []string{"deepseek-v4-flash"}, ModelMapping: map[string]string{"deepseek-v4-flash": "deepseek-v4-flash"}},
			{Name: "good-up", BaseURL: good.URL, APIKey: "k", Priority: 2, Models: []string{"deepseek-v4-flash"}, ModelMapping: map[string]string{"deepseek-v4-flash": "deepseek-v4-flash"}},
		},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules:           []config.Rule{{Model: "deepseek-v4-flash", Upstreams: []string{"bad-up", "good-up"}, Strategy: "primary_backup"}},
		},
	}
	h := New(cfg, router.New(cfg), nil)

	rec := doChat(t, h, `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hello"}]}`)
	if badCalled != 1 {
		t.Errorf("bad upstream called %d times, want 1", badCalled)
	}
	if goodCalled != 0 {
		t.Errorf("good upstream called %d times, want 0 (思考响应不应切换候选)", goodCalled)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "思考中") {
		t.Errorf("client should receive the thinking response as-is, got: %s", rec.Body.String())
	}
}

// TestChatCompletions_EmptyCompletionAllFail 验证：所有上游都返回空响应时，
// 最终返回 502（而不是把空 content 透传给客户端当成功）。
func TestChatCompletions_EmptyCompletionAllFail(t *testing.T) {
	upstream, h := newTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":null},"finish_reason":"length"}]}`))
	})
	defer upstream.Close()

	rec := doChat(t, h, `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hello"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (all upstreams returned empty completion)", rec.Code)
	}
}
