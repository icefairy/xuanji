package admin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/tidwall/gjson"
)

// opencodeSessionRe 是上游对 x-opencode-session 的校验格式。
var opencodeSessionRe = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

// 回归背景（2026-09-28）：上游管理页「测试」对 type=opencode 的上游一律 403
// FreeTierError。根因是 TestUpstream 走的是通用 APIKey 直连路径，只设了
// Authorization + Content-Type，没有补齐免费层闸门的三条件：
// ① stream=true ② tools 声明 read/shell ③ opencode 身份头（UA + x-opencode-session）。
// 线上转发路径（proxy.forward）由 applyOpencodeGate/setOpencodeHeaders 补齐，
// 测试路径漏了，于是同一个 key 在转发时正常、在测试时 403。
func TestTestUpstream_OpencodeInjectsGateAndAggregates(t *testing.T) {
	var gotBody []byte
	var gotUA, gotSession, gotAuth string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotUA = r.Header.Get("User-Agent")
		gotSession = r.Header.Get("x-opencode-session")
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// 上游只接受流式：返回 SSE 信封，网关需聚合回 chat.completion。
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"big-pickle","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"想一下"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"big-pickle","choices":[{"index":0,"delta":{"content":"你好"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer mock.Close()

	cfg := &config.Config{
		Server: config.Server{Port: 8787},
		Upstreams: []config.Upstream{
			{
				Name:    "opencode-zen",
				Type:    "opencode",
				BaseURL: mock.URL,
				APIKey:  "sk-zen-real",
				Models:  []string{"big-pickle"},
			},
		},
	}
	h, hc := newTestHandler(t, cfg)
	defer hc.Close()

	rr := httptest.NewRecorder()
	payload, _ := json.Marshal(map[string]string{"model": "big-pickle"})
	req := httptest.NewRequest(http.MethodPost, "/admin/upstreams/opencode-zen/test", strings.NewReader(string(payload)))
	req.SetPathValue("name", "opencode-zen")

	h.TestUpstream(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	// 闸门三条件
	if !gjson.GetBytes(gotBody, "stream").Bool() {
		t.Error("上游请求必须 stream=true（否则 403 FreeTierError）")
	}
	var names []string
	for _, tl := range gjson.GetBytes(gotBody, "tools").Array() {
		names = append(names, tl.Get("function.name").String())
	}
	has := func(n string) bool {
		for _, x := range names {
			if x == n {
				return true
			}
		}
		return false
	}
	if !has("read") || !has("shell") {
		t.Errorf("上游请求 tools 缺闸门桩 read/shell: %v", names)
	}
	if !strings.HasPrefix(gotUA, "opencode/") {
		t.Errorf("User-Agent = %q, 期望 opencode/ 前缀", gotUA)
	}
	if !opencodeSessionRe.MatchString(gotSession) {
		t.Errorf("x-opencode-session = %q, 不匹配 %s", gotSession, opencodeSessionRe)
	}
	// 凭证：用户填了真实 key 就用自己的
	if gotAuth != "Bearer sk-zen-real" {
		t.Errorf("Authorization = %q, 期望 Bearer sk-zen-real", gotAuth)
	}
	// 响应必须聚合回非流式，前端 testUpstream 解析 choices[0].message.content
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, rr.Body.String())
	}
	if resp["status"] != "ok" {
		t.Fatalf("status = %v, error = %v", resp["status"], resp["error"])
	}
	body, _ := resp["body"].(map[string]any)
	if body == nil {
		t.Fatalf("响应缺 body: %s", rr.Body.String())
	}
	if body["object"] != "chat.completion" {
		t.Errorf("object = %v, 期望聚合后的 chat.completion", body["object"])
	}
	msg, _ := body["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好" {
		t.Errorf("聚合内容 = %v, 期望 你好", msg["content"])
	}
}

// 未配置 API Key 的 opencode 上游：免费层匿名凭证 Bearer public。
func TestTestUpstream_OpencodeNoKeyUsesPublic(t *testing.T) {
	var gotAuth string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer mock.Close()

	cfg := &config.Config{
		Server: config.Server{Port: 8787},
		Upstreams: []config.Upstream{
			{Name: "zen-anon", Type: "opencode", BaseURL: mock.URL, Models: []string{"big-pickle"}},
		},
	}
	h, hc := newTestHandler(t, cfg)
	defer hc.Close()

	rr := httptest.NewRecorder()
	payload, _ := json.Marshal(map[string]string{"model": "big-pickle"})
	req := httptest.NewRequest(http.MethodPost, "/admin/upstreams/zen-anon/test", strings.NewReader(string(payload)))
	req.SetPathValue("name", "zen-anon")

	h.TestUpstream(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if gotAuth != "Bearer public" {
		t.Errorf("Authorization = %q, 期望 Bearer public（免费层匿名）", gotAuth)
	}
}
