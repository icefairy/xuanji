package proxy

import (
	"fmt"
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

// 本文件锁定「可重试失败」的 request_log.error_detail 必须带上游错误摘要。
//
// 背景（2026-09-25 日志实测）：stepfun 因未实名认证对每次请求回
// 403 `{"error":{"message":"real-name verification is required for your free step plan...",
// "type":"real_name_required"}}`。403 在 retry_statuses 白名单内 → 走可重试分支 →
// 中间候选失败由 `recordRetryableFailure` / chat 循环内联块落库，两者都只写
// `"retryable error: " + ferr.Error()`，而 ferr 是 `fmt.Errorf("upstream error: %s", resp.Status)`
// —— **上游的 error.message 完全丢失**：
//
//	DB 实况：error_detail = "retryable error: upstream error: 403 Forbidden"（29 条全是这样）
//	而日志同一次请求里其实有完整信息：
//	  upstream_error="message=real-name verification is required for your free step plan..."
//
// 对比：不可重试的 4xx（如 451 censorship_blocked）走 forwardOnce 的 defer，落库保留了
// `message=...censorship_blocked`。管理页「请求日志」直接展示 error_detail（admin_vue.html），
// 所以可重试失败在 UI 上只有一句「403 Forbidden」，管理员看不出「需实名认证」这类根因，
// 只能去翻 supervisor 日志。
//
// 注意：这不影响转发行为（状态码/重试/failover 都不变），仅补齐日志字段。

// TestChatCompletions_RetryableFailureRecordsUpstreamMessage 验证：可重试的 4xx/5xx
// 失败落库时，error_detail 应包含上游 error.message。
func TestChatCompletions_RetryableFailureRecordsUpstreamMessage(t *testing.T) {
	const upstreamMsg = "real-name verification is required for your free step plan"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, `{"error":{"message":%q,"type":"real_name_required"}}`, upstreamMsg)
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
			Name: "up-403", BaseURL: up.URL, APIKey: "sk-test", Weight: 100,
			Models: []string{"m"},
		}},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules:           []config.Rule{{Model: "m", Upstreams: []string{"up-403"}, Strategy: "primary_backup"}},
		},
		// 403 在重试白名单内 → 走可重试分支（线上 retry_statuses 含 403）
		Retry: config.Retry{MaxRetries: 2, RetryStatuses: []int{400, 401, 403, 404, 429, 500, 502, 503, 504}},
	}
	hc := health.New(cfg)
	defer hc.Close()
	h := New(cfg, router.New(cfg), hc)
	h.SetRecorder(rec)

	doChat(t, h, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	rec.Close() // flush

	rows, err := s.DB().Query(`SELECT status, error_detail FROM request_log WHERE endpoint='chat'`)
	if err != nil {
		t.Fatalf("query request_log: %v", err)
	}
	defer rows.Close()

	n := 0
	foundMsg := false
	for rows.Next() {
		var status int
		var detail string
		if err := rows.Scan(&status, &detail); err != nil {
			t.Fatal(err)
		}
		n++
		if strings.Contains(detail, upstreamMsg) {
			foundMsg = true
		}
	}
	if n == 0 {
		t.Fatal("可重试失败也应落库（否则请求日志页看不到这次失败）")
	}
	if !foundMsg {
		t.Errorf("可重试失败的 error_detail 未包含上游 message %q —— 管理页只能看到 "+
			"「403 Forbidden」，看不出根因（需实名认证）；"+
			"而不可重试的 451 却保留了 message，口径不一致", upstreamMsg)
	}
}

// TestRerank_NonRetryable4xxRecordsUpstreamMessage 验证：rerank 不可重试 4xx
// （403 不在重试白名单时）透传客户端的同时，error_detail 也应记下上游 message
// （与 chat 链路口径一致；不可重试的 451 同理）。
func TestRerank_NonRetryable4xxRecordsUpstreamMessage(t *testing.T) {
	const upstreamMsg = "some upstream rejection detail"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, `{"error":{"message":%q}}`, upstreamMsg)
	}))
	defer up.Close()

	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	rec := store.NewRecorder(s)

	cfg := rerankEmbedCfg(up.URL, "rerankv2m3", "BAAI/bge-reranker-v2-m3")
	// 白名单不含 403 → 走不可重试分支（透传客户端、handled=true、由 defer 落库）
	cfg.Retry.RetryStatuses = []int{400, 429, 500, 502, 503, 504}
	hc := health.New(cfg)
	defer hc.Close()
	h := New(cfg, router.New(cfg), hc)
	h.SetRecorder(rec)

	rec2 := doRerank(t, h, `{"model":"rerankv2m3","query":"apple","documents":["a"]}`)
	rec.Close()

	if rec2.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403（不可重试 4xx 应透传）", rec2.Code)
	}
	var detail string
	rows, err := s.DB().Query(`SELECT error_detail FROM request_log WHERE endpoint='rerank' ORDER BY id DESC LIMIT 1`)
	if err != nil {
		t.Fatalf("query request_log: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("rerank 不可重试 4xx 应落库")
	}
	if err := rows.Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, upstreamMsg) {
		t.Errorf("rerank 不可重试 4xx 的 error_detail 未含上游 message %q，实际 %q", upstreamMsg, detail)
	}
}

// TestRerank_RetryableFailureRecordsUpstreamMessage 同上，覆盖 rerank 链路
// （recordRetryableFailure 的另一处调用点）。
func TestRerank_RetryableFailureRecordsUpstreamMessage(t *testing.T) {
	const upstreamMsg = "Query is too long. Please provide a shorter query."
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, `{"error":{"message":%q}}`, upstreamMsg)
	}))
	defer up.Close()

	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	rec := store.NewRecorder(s)

	cfg := rerankEmbedCfg(up.URL, "rerankv2m3", "BAAI/bge-reranker-v2-m3")
	// 403 加入重试白名单（与线上 retry_statuses 一致），测试可重试分支
	cfg.Retry.RetryStatuses = []int{400, 401, 403, 404, 429, 500, 502, 503, 504}
	hc := health.New(cfg)
	defer hc.Close()
	h := New(cfg, router.New(cfg), hc)
	h.SetRecorder(rec)

	doRerank(t, h, `{"model":"rerankv2m3","query":"apple","documents":["a"]}`)
	rec.Close()

	var detail string
	rows, err := s.DB().Query(`SELECT error_detail FROM request_log WHERE endpoint='rerank' ORDER BY id DESC LIMIT 1`)
	if err != nil {
		t.Fatalf("query request_log: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("rerank 可重试失败应落库")
	}
	if err := rows.Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, upstreamMsg) {
		t.Errorf("rerank 可重试失败的 error_detail 未包含上游 message %q，实际 %q", upstreamMsg, detail)
	}
}
