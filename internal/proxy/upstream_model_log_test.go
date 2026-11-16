package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/health"
	"github.com/icefairy/xuanji/internal/router"
	"github.com/icefairy/xuanji/internal/store"
)

// 背景（2026-09-23）：request_log.upstream_model 是「按模型统计 / 费用按模型拆分」的分组键，
// 但只有 chat 链路写过它，其余端点（embed/rerank/media/video/claude/gemini/ollama）全部留空，
// 统计页用 COALESCE(...,'(未知)') 兜底后合并成一行「(未知)」——实测 35360 条（近 30 天）。
// 本文件锁定：各链路必须记录上游真实模型名。

// --- 行为测试：rerank 成功请求记录映射后的真实模型名 ---

func TestRerank_RecordUpstreamModel(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"r1","results":[{"index":0,"relevance_score":0.9}],"usage":{"total_tokens":10}}`)
	}))
	defer up.Close()

	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	rec := store.NewRecorder(s)

	cfg := rerankEmbedCfg(up.URL, "rerankv2m3", "BAAI/bge-reranker-v2-m3")
	hc := health.New(cfg)
	defer hc.Close()
	h := New(cfg, router.New(cfg), hc)
	h.SetRecorder(rec)

	if rr := doRerank(t, h, `{"model":"rerankv2m3","query":"apple","documents":["a","b"]}`); rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	rec.Close() // flush 落库

	if got := recordedUpstreamModel(t, s, "rerank"); got != "BAAI/bge-reranker-v2-m3" {
		t.Errorf("rerank upstream_model = %q, want BAAI/bge-reranker-v2-m3", got)
	}
}

// --- 行为测试：embeddings 成功请求同样记录真实模型名 ---

func TestEmbedding_RecordUpstreamModel(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1]}],"usage":{"total_tokens":5}}`)
	}))
	defer up.Close()

	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	rec := store.NewRecorder(s)

	cfg := rerankEmbedCfg(up.URL, "bge-m3", "BAAI/bge-m3")
	hc := health.New(cfg)
	defer hc.Close()
	h := New(cfg, router.New(cfg), hc)
	h.SetRecorder(rec)

	if rr := doEmbed(t, h, `{"model":"bge-m3","input":"hello"}`); rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	rec.Close()

	if got := recordedUpstreamModel(t, s, "embed"); got != "BAAI/bge-m3" {
		t.Errorf("embed upstream_model = %q, want BAAI/bge-m3", got)
	}
}

// --- 行为测试：chat 中间候选失败在真实模型名缺失时回退客户端模型名 ---
//
// 触发条件：上游的 model_mapping 值为空串（realModels 展开得到空串，
// pickAvailableModel 返回空）→ 若不兜底，记录里 upstream_model 为空，
// 统计页显示「(未知)」。空映射是线上 chat 空值行的实际来源之一。

func TestChatFailure_RecordUpstreamModelFallback(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests) // 429：可重试 → 走中间失败补记分支
	}))
	defer up.Close()

	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	rec := store.NewRecorder(s)

	cfg := &config.Config{
		Upstreams: []config.Upstream{{
			Name:     "up-empty-map",
			BaseURL:  up.URL + "/v1",
			APIKey:   "sk-test",
			Priority: 10,
			Models:   []string{"deepseek-v4-flash"},
			// 空映射：realModels 展开得到空串 → pickAvailableModel 返回空串
			ModelMapping: map[string]string{"deepseek-v4-flash": ""},
		}},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules: []config.Rule{{
				Model:     "deepseek-v4-flash",
				Upstreams: []string{"up-empty-map"},
				Strategy:  "primary_backup",
			}},
		},
		Retry: config.Retry{MaxRetries: 1, RetryStatuses: []int{429}},
	}
	hc := health.New(cfg)
	defer hc.Close()
	h := New(cfg, router.New(cfg), hc)
	h.SetRecorder(rec)

	doChat(t, h, `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`)
	rec.Close()

	rows, err := s.DB().Query(`SELECT COALESCE(upstream_model,'') FROM request_log WHERE endpoint='chat'`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var mdl string
		if err := rows.Scan(&mdl); err != nil {
			t.Fatal(err)
		}
		n++
		if mdl == "" {
			t.Error("chat 失败记录的 upstream_model 为空（统计页会显示「(未知)」）")
		}
	}
	if n == 0 {
		t.Fatal("chat 失败请求未落库")
	}
}

// --- 失败补记路径：rerank 上游 500 全部候选失败时，补记行也带真实模型名 ---
// （之前 recordRetryableFailure 传入的 failedModel 可能为空串 → 统计页「(未知)」）

func TestRerankFailure_RecordUpstreamModel(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer up.Close()

	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	rec := store.NewRecorder(s)

	cfg := rerankEmbedCfg(up.URL, "rerankv2m3", "BAAI/bge-reranker-v2-m3")
	hc := health.New(cfg)
	defer hc.Close()
	h := New(cfg, router.New(cfg), hc)
	h.SetRecorder(rec)

	if rr := doRerank(t, h, `{"model":"rerankv2m3","query":"apple","documents":["a"]}`); rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rr.Code, rr.Body.String())
	}
	rec.Close()

	if got := recordedUpstreamModel(t, s, "rerank"); got != "BAAI/bge-reranker-v2-m3" {
		t.Errorf("rerank 失败行 upstream_model = %q, want BAAI/bge-reranker-v2-m3", got)
	}
}

// recordedUpstreamModel 返回指定端点最近一条记录的 upstream_model。
func recordedUpstreamModel(t *testing.T, s *store.Store, endpoint string) string {
	t.Helper()
	var mdl string
	err := s.DB().QueryRow(
		`SELECT COALESCE(upstream_model,'') FROM request_log WHERE endpoint=? ORDER BY id DESC LIMIT 1`,
		endpoint).Scan(&mdl)
	if err != nil {
		t.Fatalf("查询 %s 记录失败: %v", endpoint, err)
	}
	return mdl
}
