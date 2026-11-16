package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/health"
	"github.com/icefairy/xuanji/internal/router"
)

// rerankEmbedCfg 构造一个 rerank/embeddings 双用的单上游配置（含健康检查器）。
func rerankEmbedCfg(upURL, model, realModel string) *config.Config {
	return &config.Config{
		Upstreams: []config.Upstream{{
			Name:         "up-rr",
			BaseURL:      upURL + "/v1",
			APIKey:       "sk-test",
			Priority:     10,
			Weight:       100,
			Models:       []string{model},
			ModelMapping: map[string]string{model: realModel},
		}},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules:           []config.Rule{{Model: model, Upstreams: []string{"up-rr"}, Strategy: "primary_backup"}},
		},
		// 400 在重试白名单内（与线上 retry_statuses 一致）：走可重试分支（原 bug 触发点）
		Retry: config.Retry{MaxRetries: 1, RetryStatuses: []int{400, 429, 500, 502, 503, 504}},
	}
}

// TestRerank_ClientRequestErrorNotMarkFailure 验证 rerank 上游因「客户端请求内容问题」
// 回 400（空 documents / 缺必填字段）时不计入上游健康失败。
//
// 背景（2026-09-13 日志实测）：客户端对 rerank 传空 documents/缺字段，上游回
// 400 {"code":20015,"message":"List should have at least 1 item after validation"}。
// 400 在 retry_statuses 白名单内 → forwardRerank 可重试分支无条件
// MarkFailedWithReason + 循环 MarkFailure，把 免费小模型/免费小模型-lxr/cfcdn
// 三个健康上游各打成 degraded（当天 97 次 rerank 400，三个上游各被拉黑 34/34/35 次）。
// 直连上游同参数正常请求返回 200，证明上游本身健康。
//
// 修复：识别为客户端请求错误时不标 fastfail、不计健康失败（保留可重试语义）。
func TestRerank_ClientRequestErrorNotMarkFailure(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"code":20015,"message":"List should have at least 1 item after validation, not 0","data":null}`)
	}))
	defer up.Close()

	cfg := rerankEmbedCfg(up.URL, "rerankv2m3", "BAAI/bge-reranker-v2-m3")
	hc := health.New(cfg)
	defer hc.Close()
	h := New(cfg, router.New(cfg), hc)
	ff := NewFastFailCache(5 * 60 * 1e9)
	h.SetFastFail(ff)

	rec := doRerank(t, h, `{"model":"rerankv2m3","query":"apple","documents":[]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (all candidates failed); body=%s", rec.Code, rec.Body.String())
	}
	if st := hc.Status("up-rr"); st != health.StateHealthy {
		t.Errorf("health status = %q, want healthy（rerank 400 是客户端请求内容问题，不是上游故障）", st)
	}
	if ff.IsBlacklisted("up-rr", "BAAI/bge-reranker-v2-m3") {
		t.Error("upstream should not be fastfail-blacklisted for a client request error")
	}
	if hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1", hits.Load())
	}
}

// TestEmbeddings_ClientRequestErrorNotMarkFailure 验证 embeddings 上游因客户端请求内容
// 问题回 400（空 input 数组等）时不计入上游健康失败（2026-09-13 实测 6 例）。
func TestEmbeddings_ClientRequestErrorNotMarkFailure(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"code":20015,"message":"The parameter is invalid. Please check again.","data":null}`)
	}))
	defer up.Close()

	cfg := rerankEmbedCfg(up.URL, "bge-m3", "BAAI/bge-m3")
	hc := health.New(cfg)
	defer hc.Close()
	h := New(cfg, router.New(cfg), hc)
	ff := NewFastFailCache(5 * 60 * 1e9)
	h.SetFastFail(ff)

	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"bge-m3","input":[]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Embeddings(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (all candidates failed); body=%s", rec.Code, rec.Body.String())
	}
	if st := hc.Status("up-rr"); st != health.StateHealthy {
		t.Errorf("health status = %q, want healthy（embeddings 400 是客户端请求内容问题，不是上游故障）", st)
	}
	if ff.IsBlacklisted("up-rr", "BAAI/bge-m3") {
		t.Error("upstream should not be fastfail-blacklisted for a client request error")
	}
	if hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1", hits.Load())
	}
}

// TestRerank_Upstream5xxStillMarkedFailure 回归保护：真实上游故障（5xx）必须仍被记为
// 上游失败并拉黑——修复只应豁免「客户端请求问题」，不能放宽真实故障的判定。
func TestRerank_Upstream5xxStillMarkedFailure(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"error":{"message":"bad gateway"}}`)
	}))
	defer up.Close()

	cfg := rerankEmbedCfg(up.URL, "rerankv2m3", "BAAI/bge-reranker-v2-m3")
	hc := health.New(cfg)
	defer hc.Close()
	h := New(cfg, router.New(cfg), hc)
	ff := NewFastFailCache(5 * 60 * 1e9)
	h.SetFastFail(ff)

	doRerank(t, h, `{"model":"rerankv2m3","query":"apple","documents":["a"]}`)

	if st := hc.Status("up-rr"); st == health.StateHealthy {
		t.Error("5xx 是真实上游故障，必须仍被计为失败（degraded）")
	}
	if !ff.IsBlacklisted("up-rr", "BAAI/bge-reranker-v2-m3") {
		t.Error("5xx 真实故障必须仍进 fastfail 黑名单")
	}
}
