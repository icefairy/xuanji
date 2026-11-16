package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/icefairy/xuanji/internal/health"
	"github.com/icefairy/xuanji/internal/router"
	"github.com/icefairy/xuanji/internal/store"
)

// TestRerank_FailedRequestIsRecorded 回归测试（2026-09-15 实测）：
// rerank / embeddings 的**失败请求完全不落库**——日志有 51 次 rerank 请求（含 16 次
// status=502），但 request_log 里 rerank 非 200 记录 **0 条**（同窗口 chat 的失败
// 都有行）。原因：forwardRerank/forwardEmbedding 的 defer 只在 handled=true 时记录，
// 而终端失败（全部候选用尽 → writeError 502）与中间候选失败都满足 handled=false；
// 且 Rerank/Embeddings 循环没有 chat 那样的中间失败补记（2026-08-29 提交 66feb25
// 只给 ChatCompletions 加了补记）。
//
// 危害：管理页请求日志/成功率统计对 rerank/embeddings **永远显示 100% 成功**，
// 排障时看不到失败（本次靠日志与 DB 行数比对才发现）。
func TestRerank_FailedRequestIsRecorded(t *testing.T) {
	// 上游返回客户端请求类 400（retryable 白名单内）→ 全部候选失败 → 网关 502
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"code":20015,"message":"List should have at least 1 item after validation, not 0","data":null}`)
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

	rr := doRerank(t, h, `{"model":"rerankv2m3","query":"apple","documents":[]}`)
	rec.Close() // flush 落库

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rr.Code, rr.Body.String())
	}

	rows, err := s.DB().Query(`SELECT endpoint, status, error_detail FROM request_log WHERE endpoint='rerank'`)
	if err != nil {
		t.Fatalf("query request_log: %v", err)
	}
	defer rows.Close()

	n, withErr := 0, 0
	for rows.Next() {
		var endpoint, detail string
		var status int
		if err := rows.Scan(&endpoint, &status, &detail); err != nil {
			t.Fatal(err)
		}
		n++
		if status != 200 && strings.Contains(detail, "400") {
			withErr++
		}
	}
	if n == 0 {
		t.Fatal("rerank 失败请求未落库：request_log 无 rerank 记录（日志有 502 但 DB 无行）")
	}
	if withErr == 0 {
		t.Errorf("rerank 失败请求落库但未见失败行（应含 status=400 与 error_detail）；共 %d 行", n)
	}
}

// TestEmbeddings_FailedRequestIsRecorded 同上，覆盖 embeddings 链路。
func TestEmbeddings_FailedRequestIsRecorded(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"code":20015,"message":"The parameter is invalid. Please check again.","data":null}`)
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

	rr := doEmbed(t, h, `{"model":"bge-m3","input":[]}`)
	rec.Close()

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rr.Code, rr.Body.String())
	}

	rows, err := s.DB().Query(`SELECT status, error_detail FROM request_log WHERE endpoint='embed'`)
	if err != nil {
		t.Fatalf("query request_log: %v", err)
	}
	defer rows.Close()

	n := 0
	for rows.Next() {
		var status int
		var detail string
		if err := rows.Scan(&status, &detail); err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n == 0 {
		t.Fatal("embeddings 失败请求未落库：request_log 无 embed 记录")
	}
}

// TestRerank_SuccessStillRecordedOnce 反向保护：成功请求仍只记一条 200，不能因修复重复记录。
func TestRerank_SuccessStillRecordedOnce(t *testing.T) {
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

	rr := doRerank(t, h, `{"model":"rerankv2m3","query":"apple","documents":["a","b"]}`)
	rec.Close()

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var n, bad int
	rows, _ := s.DB().Query(`SELECT status FROM request_log WHERE endpoint='rerank'`)
	defer rows.Close()
	for rows.Next() {
		var st int
		rows.Scan(&st)
		n++
		if st != 200 {
			bad++
		}
	}
	if n != 1 {
		t.Errorf("成功请求应恰好落库 1 条，实际 %d 条", n)
	}
	if bad != 0 {
		t.Errorf("成功请求不应有非 200 行，实际 %d 条", bad)
	}
}
