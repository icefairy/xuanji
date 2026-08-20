package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/proxy"
	"github.com/icefairy/xuanji/internal/router"
)

// TestChat_RoutingReportsActualUpstream 验证对话调试 /admin/chat 的 routing.upstream
// 返回的是实际转发的上游（跳过禁用的第一候选，命中商汤），而非规则里固定的第一候选。
// 此前展示用自行复刻的路由判断，与 proxy 实际转发不一致。
func TestChat_RoutingReportsActualUpstream(t *testing.T) {
	// 商汤 mock 上游
	bsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer bsrv.Close()

	cfg := testConfig()
	// 规则第一候选 disabled，第二候选（商汤）才是实际可用的
	cfg.Upstreams = append(cfg.Upstreams,
		config.Upstream{Name: "disabled-a", Type: "openai", BaseURL: "http://x.local", APIKey: "sk-x", Enabled: false, Models: []string{"flash"}},
		config.Upstream{Name: "shangtang-b", Type: "openai", BaseURL: bsrv.URL + "/v1", APIKey: "sk-b", Enabled: true, Models: []string{"flash"}},
	)
	cfg.Routing.Rules = append(cfg.Routing.Rules, config.Rule{Model: "flash", Upstreams: []string{"disabled-a", "shangtang-b"}})

	h, hc := newTestHandler(t, cfg)
	defer hc.Close()
	rt := router.New(cfg)
	px := proxy.New(cfg, rt, nil)
	h.SetProxy(px, rt)

	body := `{"model":"flash","messages":[{"role":"user","content":"hi"}],"max_tokens":64}`
	req := httptest.NewRequest(http.MethodPost, "/admin/chat", strings.NewReader(body))
	rr := httptest.NewRecorder()
	h.Chat(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Reply   string         `json:"reply"`
		Routing map[string]any `json:"routing"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Routing["upstream"] != "shangtang-b" {
		t.Fatalf("routing.upstream = %v, want shangtang-b（实际命中，跳过 disabled-a）", resp.Routing["upstream"])
	}
	if resp.Reply != "hi" {
		t.Fatalf("reply = %q, want hi", resp.Reply)
	}
}
