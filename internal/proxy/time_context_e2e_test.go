package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/health"
	"github.com/icefairy/xuanji/internal/router"
)

func e2eConfig(srvURL string) *config.Config {
	return &config.Config{
		Upstreams: []config.Upstream{{
			Name: "test-up", BaseURL: srvURL + "/v1", APIKey: "sk-test",
			Priority: 1, Weight: 100, Models: []string{"deepseek-v4-flash"},
		}},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules:           []config.Rule{{Model: "deepseek-v4-flash", Upstreams: []string{"test-up"}, Strategy: "primary_backup"}},
		},
		Retry: config.Retry{MaxRetries: 3, RetryStatuses: []int{429, 500, 502, 503, 504}},
	}
}

func captureUpstream(t *testing.T, got *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*got = append(*got, string(b))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	}))
}

// TestTimeContext_E2E_TwoTurns 验证真实的「两轮对话」：第二轮应带「距上轮」。
// 这是本功能的核心验收点——不是只看当前时间，而是能回答「过了多久」。
func TestTimeContext_E2E_TwoTurns(t *testing.T) {
	var bodies []string
	srv := captureUpstream(t, &bodies)
	defer srv.Close()

	cfg := e2eConfig(srv.URL)
	cfg.Proxy.TimeContext = config.TimeContextConfig{
		Enabled: true, TZ: "Asia/Shanghai", Format: "%Y-%m-%d %H:%M", MinGapMinutes: 5,
		MaxSessions: 100,
	}
	store := NewTimeContextStore(100)
	h := New(cfg, router.New(cfg), health.New(cfg))
	h.SetTimeContextStore(store)

	send := func(body string) {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ChatCompletions(rec, req)
		if rec.Code != 200 {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	}

	// 第一轮：单条 user。客户端此时 history 只有这一条。
	send(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"第一句"}]}`)

	// 制造真实间隔：手动把会话记录的 LastSeen 往前推 3 小时 22 分
	for _, k := range store.Keys() {
		ent, _ := store.Get(k)
		store.Touch(k, ent.TailKey, time.Now().Add(-(3*time.Hour + 22*time.Minute)), 0)
	}

	// 第二轮：客户端 history = [user1, assistant1, user2]（这是客户端的真实行为）
	send(`{"model":"deepseek-v4-flash","messages":[` +
		`{"role":"user","content":"第一句"},` +
		`{"role":"assistant","content":"收到"},` +
		`{"role":"user","content":"三小时后我问"}]}`)

	if len(bodies) != 2 {
		t.Fatalf("upstream 收到 %d 个请求, want 2", len(bodies))
	}
	// 第一轮：有时间，无止上轮
	var b1 struct {
		Messages []struct{ Content string } `json:"messages"`
	}
	json.Unmarshal([]byte(bodies[0]), &b1)
	if !strings.Contains(b1.Messages[0].Content, "【时间】") {
		t.Errorf("第一轮缺时间行: %q", b1.Messages[0].Content)
	}
	if strings.Contains(b1.Messages[0].Content, "距上轮") {
		t.Errorf("第一轮不应有距上轮: %q", b1.Messages[0].Content)
	}

	// 第二轮：最后一条有时间 + 距上轮；历史消息保持原样
	var b2 struct {
		Messages []struct{ Content string } `json:"messages"`
	}
	json.Unmarshal([]byte(bodies[1]), &b2)
	if len(b2.Messages) != 3 {
		t.Fatalf("第二轮 messages 数 = %d", len(b2.Messages))
	}
	if b2.Messages[0].Content != "第一句" {
		t.Errorf("历史消息被改动（会破坏前缀缓存）: %q", b2.Messages[0].Content)
	}
	if b2.Messages[1].Content != "收到" {
		t.Errorf("历史 assistant 消息被改动: %q", b2.Messages[1].Content)
	}
	last := b2.Messages[2].Content
	if !strings.Contains(last, "【时间】") {
		t.Errorf("第二轮缺时间行: %q", last)
	}
	if !strings.Contains(last, "距上轮") {
		t.Errorf("第二轮缺距上轮（核心功能失效）: %q", last)
	}
	if !strings.Contains(last, "小时") {
		t.Errorf("间隔渲染不对: %q", last)
	}
}

// TestTimeContext_E2E_Disabled 未开启时必须零改动透传。
func TestTimeContext_E2E_Disabled(t *testing.T) {
	var bodies []string
	srv := captureUpstream(t, &bodies)
	defer srv.Close()

	cfg := e2eConfig(srv.URL)
	cfg.Proxy.TimeContext = config.TimeContextConfig{Enabled: false}
	h := New(cfg, router.New(cfg), health.New(cfg))
	h.SetTimeContextStore(NewTimeContextStore(100))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"现在几点"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ChatCompletions(rec, req)

	var got struct {
		Messages []struct{ Content string } `json:"messages"`
	}
	json.Unmarshal([]byte(bodies[0]), &got)
	if got.Messages[0].Content != "现在几点" {
		t.Errorf("未开启时不应注入: %q", got.Messages[0].Content)
	}
}
