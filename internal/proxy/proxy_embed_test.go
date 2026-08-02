package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/router"
)

func doEmbed(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Embeddings(rec, req)
	return rec
}

// TestEmbeddings_OpenAIUpstream 验证 /v1/embeddings 转发到 OpenAI 兼容上游成功
// （回归：此前非 ollama 上游嵌入请求被 ollama handler 502）。
func TestEmbeddings_OpenAIUpstream(t *testing.T) {
	upstream, h := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("upstream path = %q, want /v1/embeddings", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"model":"deepseek-ai/DeepSeek-V4-Flash"}`)
	})
	defer upstream.Close()

	rec := doEmbed(t, h, `{"model":"deepseek-v4-flash","input":"hello"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "embedding") {
		t.Errorf("response should contain embedding data, got: %s", rec.Body.String())
	}
}

// TestEmbeddings_Failover 验证 embeddings 第一个上游失败时切换候选。
func TestEmbeddings_Failover(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[1.0]}]}`)
	}))
	defer good.Close()

	cfg := &config.Config{
		Retry: config.Retry{MaxRetries: 3, RetryStatuses: []int{429, 500}},
		Upstreams: []config.Upstream{
			{Name: "bad-up", BaseURL: bad.URL, APIKey: "sk-test", Priority: 1, Models: []string{"deepseek-v4-flash"}, ModelMapping: map[string]string{"deepseek-v4-flash": "deepseek-v4-flash"}},
			{Name: "good-up", BaseURL: good.URL, APIKey: "sk-test", Priority: 2, Models: []string{"deepseek-v4-flash"}, ModelMapping: map[string]string{"deepseek-v4-flash": "deepseek-v4-flash"}},
		},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules:           []config.Rule{{Model: "deepseek-v4-flash", Upstreams: []string{"bad-up", "good-up"}, Strategy: "primary_backup"}},
		},
	}
	h := New(cfg, router.New(cfg), nil)

	rec := doEmbed(t, h, `{"model":"deepseek-v4-flash","input":"hello"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after failover", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "1.0") {
		t.Errorf("should receive good upstream embedding, got: %s", rec.Body.String())
	}
}
