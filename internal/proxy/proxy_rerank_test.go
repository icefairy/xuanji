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

func doRerank(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/rerank", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Rerank(rec, req)
	return rec
}

// TestRerank_OpenAIUpstream 验证 /v1/rerank 转发到 OpenAI 兼容上游成功，
// 且 model_mapping 生效（客户端简单名 → 上游真实名）。
func TestRerank_OpenAIUpstream(t *testing.T) {
	upstream, h := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/rerank" {
			t.Errorf("upstream path = %q, want /v1/rerank", r.URL.Path)
		}
		// 验证 model 已被映射为上游真实名
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		body := string(buf[:n])
		if !strings.Contains(body, `"model":"deepseek-ai/DeepSeek-V4-Flash"`) {
			t.Errorf("upstream received model not mapped, body: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"rerank-1","results":[{"index":0,"relevance_score":0.92},{"index":1,"relevance_score":0.85}],"usage":{"total_tokens":120}}`)
	})
	defer upstream.Close()

	rec := doRerank(t, h, `{"model":"deepseek-v4-flash","query":"apple","documents":["fruit","car"],"top_n":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "relevance_score") {
		t.Errorf("response should contain results, got: %s", rec.Body.String())
	}
}

// TestRerank_MissingModel 验证缺 model 返回 400。
func TestRerank_MissingModel(t *testing.T) {
	_, h := newTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{}`)
	})

	rec := doRerank(t, h, `{"query":"apple","documents":["a","b"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestRerank_Failover 验证第一个上游失败时切换候选。
func TestRerank_Failover(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"r","results":[{"index":0,"relevance_score":0.99}],"usage":{"total_tokens":10}}`)
	}))
	defer good.Close()

	cfg := &config.Config{
		Retry: config.Retry{MaxRetries: 3, RetryStatuses: []int{429, 500}},
		Upstreams: []config.Upstream{
			{Name: "bad-up", BaseURL: bad.URL, APIKey: "key", Priority: 1, Models: []string{"bge-reranker"}, ModelMapping: map[string]string{"bge-reranker": "bge-reranker"}},
			{Name: "good-up", BaseURL: good.URL, APIKey: "key", Priority: 2, Models: []string{"bge-reranker"}, ModelMapping: map[string]string{"bge-reranker": "bge-reranker"}},
		},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules:           []config.Rule{{Model: "bge-reranker", Upstreams: []string{"bad-up", "good-up"}, Strategy: "primary_backup"}},
		},
	}
	h := New(cfg, router.New(cfg), nil)

	rec := doRerank(t, h, `{"model":"bge-reranker","query":"q","documents":["a"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after failover", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "0.99") {
		t.Errorf("should receive good upstream result, got: %s", rec.Body.String())
	}
}
