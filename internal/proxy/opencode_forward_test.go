package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/router"
	"github.com/tidwall/gjson"
)

// newOpencodeHandler 构造一个 type=opencode 的 Handler，指向 mock 上游。
func newOpencodeHandler(t *testing.T, upstreamFn http.HandlerFunc) (*httptest.Server, *Handler) {
	t.Helper()
	upstream := httptest.NewServer(upstreamFn)
	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{
				Name:    "zen",
				Type:    "opencode",
				BaseURL: upstream.URL,
				Models:  []string{"big-pickle"},
			},
		},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules: []config.Rule{
				{Model: "big-pickle", Upstreams: []string{"zen"}, Strategy: "primary_backup"},
			},
		},
	}
	return upstream, New(cfg, router.New(cfg), nil)
}

// TestOpencodeForward_InjectsGateForStreaming 验证流式客户端：
// 网关补齐 tools 与身份头，且用匿名 public 凭证。
func TestOpencodeForward_InjectsGateForStreaming(t *testing.T) {
	var gotBody []byte
	upstream, h := newOpencodeHandler(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		if got := r.Header.Get("Authorization"); got != "Bearer public" {
			t.Errorf("Authorization = %q, 期望 Bearer public（未配 key 时匿名）", got)
		}
		if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "opencode/") {
			t.Errorf("User-Agent = %q, 期望 opencode/ 前缀", ua)
		}
		if sid := r.Header.Get("x-opencode-session"); !strings.HasPrefix(sid, "ses_") {
			t.Errorf("x-opencode-session = %q, 期望 ses_ 前缀", sid)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"model\":\"big-pickle\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	})
	defer upstream.Close()

	rec := doChat(t, h, `{"model":"big-pickle","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	names := opencodeToolNames(gotBody)
	if !names["read"] || !names["shell"] {
		t.Errorf("上游收到的 tools 缺桩: %v", names)
	}
	if !gjson.GetBytes(gotBody, "stream").Bool() {
		t.Error("上游收到的 stream 应为 true")
	}
	if got := gjson.GetBytes(gotBody, "tool_choice").String(); got != "none" {
		t.Errorf("tool_choice = %q, 期望 none（客户端无工具）", got)
	}
}

// TestOpencodeForward_AggregatesNonStream 验证非流式客户端：
// 网关对上游仍发流式（闸门条件①），并把 SSE 聚合成 chat.completion 返回。
func TestOpencodeForward_AggregatesNonStream(t *testing.T) {
	var gotBody []byte
	upstream, h := newOpencodeHandler(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"big-pickle\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"he\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"big-pickle\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"llo\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	defer upstream.Close()

	rec := doChat(t, h, `{"model":"big-pickle","messages":[{"role":"user","content":"hello"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !gjson.GetBytes(gotBody, "stream").Bool() {
		t.Error("上游请求必须为 stream=true（非流式会被上游 403）")
	}
	out := rec.Body.Bytes()
	if got := gjson.GetBytes(out, "object").String(); got != "chat.completion" {
		t.Fatalf("object = %q, 期望聚合后的 chat.completion；body=%s", got, rec.Body.String())
	}
	if got := gjson.GetBytes(out, "choices.0.message.content").String(); got != "hello" {
		t.Errorf("聚合内容 = %q, 期望 hello", got)
	}
	if got := gjson.GetBytes(out, "usage.total_tokens").Int(); got != 5 {
		t.Errorf("usage.total_tokens = %d, 期望 5", got)
	}
}

// TestOpencodeForward_KeepsClientTools 验证真实 agent 自带工具时不被降级：
// 保留客户端 tool_choice，仅补缺失的桩。
func TestOpencodeForward_KeepsClientTools(t *testing.T) {
	var gotBody []byte
	upstream, h := newOpencodeHandler(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"model\":\"big-pickle\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	})
	defer upstream.Close()

	rec := doChat(t, h, `{"model":"big-pickle","messages":[{"role":"user","content":"hi"}],"stream":true,`+
		`"tools":[{"type":"function","function":{"name":"Bash","parameters":{"type":"object"}}}],"tool_choice":"auto"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := gjson.GetBytes(gotBody, "tool_choice").String(); got != "auto" {
		t.Errorf("tool_choice = %q, 期望保持 auto", got)
	}
	names := opencodeToolNames(gotBody)
	for _, want := range []string{"Bash", "read", "shell"} {
		if !names[want] {
			t.Errorf("上游收到的 tools 缺 %s: %v", want, names)
		}
	}
}
