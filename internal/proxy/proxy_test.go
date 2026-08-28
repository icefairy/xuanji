package proxy

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/health"
	"github.com/icefairy/xuanji/internal/router"
	"github.com/icefairy/xuanji/internal/store"
	"github.com/tidwall/gjson"
)

// newTestHandler 构造一个指向 mock 上游的 Handler。
func newTestHandler(t *testing.T, upstreamFn http.HandlerFunc) (*httptest.Server, *Handler) {
	t.Helper()
	upstream := httptest.NewServer(upstreamFn)
	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{
				Name:         "up",
				BaseURL:      upstream.URL + "/v1",
				APIKey:       "sk-test",
				Priority:     10,
				Models:       []string{"deepseek-v4-flash"},
				ModelMapping: map[string]string{"deepseek-v4-flash": "deepseek-ai/DeepSeek-V4-Flash"},
			},
		},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules: []config.Rule{
				{Model: "deepseek-v4-flash", Upstreams: []string{"up"}, Strategy: "primary_backup"},
			},
		},
	}
	return upstream, New(cfg, router.New(cfg), nil)
}

func doChat(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ChatCompletions(rec, req)
	return rec
}

func TestChatCompletions_ModelMappingApplied(t *testing.T) {
	upstream, h := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization = %q, want Bearer sk-test", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		data, _ := io.ReadAll(r.Body)
		if got := gjson.GetBytes(data, "model").String(); got != "deepseek-ai/DeepSeek-V4-Flash" {
			t.Errorf("upstream received model = %q, want deepseek-ai/DeepSeek-V4-Flash", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"up-1","object":"chat.completion","model":"deepseek-ai/DeepSeek-V4-Flash","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	})
	defer upstream.Close()

	rec := doChat(t, h, `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hello"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := gjson.Get(rec.Body.String(), "id").String(); got != "up-1" {
		t.Errorf("response id = %q, want up-1 (passthrough)", got)
	}
}

func TestChatCompletions_NonStreamingPassthrough(t *testing.T) {
	const upstreamBody = `{"id":"x","object":"chat.completion","model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`
	upstream, h := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, upstreamBody)
	})
	defer upstream.Close()

	rec := doChat(t, h, `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hello"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != upstreamBody {
		t.Errorf("body not passthrough:\n got=%s\nwant=%s", got, upstreamBody)
	}
}

func TestChatCompletions_StreamingSSE(t *testing.T) {
	const sse = "data: {\"id\":\"s1\",\"choices\":[{\"delta\":{\"content\":\"hel\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	upstream, h := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, sse)
	})
	defer upstream.Close()

	rec := doChat(t, h, `{"model":"deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := rec.Body.String(); got != sse {
		t.Errorf("SSE body not passthrough:\n got=%q\nwant=%q", got, sse)
	}
}

func TestChatCompletions_UpstreamErrorMapping(t *testing.T) {
	upstream, h := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"quota exhausted","type":"rate_limit_error","code":"rate_limit_exceeded"}}`)
	})
	defer upstream.Close()

	rec := doChat(t, h, `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hello"}]}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	msg := gjson.Get(rec.Body.String(), "error.message").String()
	if msg != "quota exhausted" {
		t.Errorf("error.message = %q, want quota exhausted", msg)
	}
	if got := gjson.Get(rec.Body.String(), "error.type").String(); got != "rate_limit_error" {
		t.Errorf("error.type = %q, want rate_limit_error", got)
	}
}

func TestChatCompletions_Upstream5xxMapping(t *testing.T) {
	upstream, h := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `internal boom`)
	})
	defer upstream.Close()

	rec := doChat(t, h, `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hello"}]}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := gjson.Get(rec.Body.String(), "error.type").String(); got != "server_error" {
		t.Errorf("error.type = %q, want server_error", got)
	}
}

func TestChatCompletions_NoRoute(t *testing.T) {
	_, h := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called for unmapped model")
	})
	rec := doChat(t, h, `{"model":"unknown-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got := gjson.Get(rec.Body.String(), "error.code").String(); got != "model_not_found" {
		t.Errorf("error.code = %q, want model_not_found", got)
	}
}

func TestChatCompletions_MissingModel(t *testing.T) {
	_, h := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called without model")
	})
	rec := doChat(t, h, `{"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestChatCompletions_UpstreamUnreachable(t *testing.T) {
	upstream, h := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {})
	upstream.Close() // 立即关闭，制造连接失败

	rec := doChat(t, h, `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if got := gjson.Get(rec.Body.String(), "error.code").String(); got != "upstream_unreachable" {
		t.Errorf("error.code = %q, want upstream_unreachable", got)
	}
}

// cancelRoundTripper 模拟上游返回 context.Canceled（客户端断连导致 Do 被取消）。
type cancelRoundTripper struct{}

func (cancelRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, context.Canceled
}

// TestChatCompletions_ClientCancelNotFastFail 验证：客户端 context 取消时，
// 不应把候选上游标记进 fastfail 黑名单（2026-08-02 修复）。
// 回归场景：一次客户端断连曾把所有候选全拉黑 60 分钟。
func TestChatCompletions_ClientCancelNotFastFail(t *testing.T) {
	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{Name: "up", BaseURL: "http://127.0.0.1:1", APIKey: "k", Priority: 10, Models: []string{"deepseek-v4-flash"}},
		},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules: []config.Rule{
				{Model: "deepseek-v4-flash", Upstreams: []string{"up"}, Strategy: "primary_backup"},
			},
		},
	}
	h := New(cfg, router.New(cfg), nil)
	// 用假 RoundTripper 模拟 Do 返回 context.Canceled（不依赖真实 TCP 连接）
	h.client = &http.Client{Transport: cancelRoundTripper{}}
	ff := NewFastFailCache(time.Minute)
	h.SetFastFail(ff)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ChatCompletions(rec, req)

	if h.fastFail.IsBlacklisted("up", "deepseek-v4-flash") {
		t.Fatal("upstream must NOT be blacklisted after client cancel")
	}
}

// TestChatCompletions_ClientCancelBeforeAttempt 验证：客户端在转发前已取消，
// 重试循环直接中止，不尝试任何上游、不标记 fastfail。
func TestChatCompletions_ClientCancelBeforeAttempt(t *testing.T) {
	called := false
	upstream, h := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	})
	defer upstream.Close()

	ff := NewFastFailCache(time.Minute)
	h.SetFastFail(ff)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 转发前就取消
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ChatCompletions(rec, req)

	if called {
		t.Fatal("upstream must not be called when client already disconnected")
	}
	if h.fastFail.IsBlacklisted("up", "deepseek-v4-flash") {
		t.Fatal("upstream must NOT be blacklisted when client disconnected before attempt")
	}
}

// TestChatCompletions_AllConnErrorsClearsBlacklist 验证：全部候选失败且为连接类错误
// （本地网络断）时，清空 fastfail 黑名单——网络恢复后立即可用，不用等 probe。
func TestChatCompletions_AllConnErrorsClearsBlacklist(t *testing.T) {
	// 两个上游，全部指向一个已关闭的端口 → 连接拒绝（net.Error）
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{Name: "up1", BaseURL: srv1.URL, APIKey: "k", Priority: 1, Models: []string{"m"}},
			{Name: "up2", BaseURL: srv2.URL, APIKey: "k", Priority: 2, Models: []string{"m"}},
		},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules: []config.Rule{
				{Model: "m", Upstreams: []string{"up1", "up2"}, Strategy: "primary_backup"},
			},
		},
	}
	h := New(cfg, router.New(cfg), nil)
	srv1.Close() // 连接拒绝
	srv2.Close()

	ff := NewFastFailCache(time.Minute)
	h.SetFastFail(ff)

	// 先手动标记两个上游进黑名单（模拟之前失败的标记）
	ff.MarkFailed("up1", "m")
	ff.MarkFailed("up2", "m")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ChatCompletions(rec, req)

	// 连接类错误全失败 → 黑名单应被清空
	if ff.IsBlacklisted("up1", "m") {
		t.Error("up1 must be cleared after all-conn-error failure")
	}
	if ff.IsBlacklisted("up2", "m") {
		t.Error("up2 must be cleared after all-conn-error failure")
	}
}

func TestParseUsageCacheFallback(t *testing.T) {
	// 场景1：上游返回 DeepSeek 系字段（完整缓存信息）
	var hit1, miss1, pt1, ct1 int64
	ok := parseUsage([]byte(`{"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_cache_hit_tokens":60,"prompt_cache_miss_tokens":40}}`), &pt1, &ct1, &hit1, &miss1)
	if !ok || hit1 != 60 || miss1 != 40 || pt1 != 100 {
		t.Errorf("scenario1 failed: ok=%v hit=%d miss=%d pt=%d", ok, hit1, miss1, pt1)
	}
	// 场景2：OpenAI 标准 cached_tokens（无 miss 字段 → 兜底 miss=pt-hit）
	var hit2, miss2, pt2, ct2 int64
	ok = parseUsage([]byte(`{"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":60}}}`), &pt2, &ct2, &hit2, &miss2)
	if !ok || hit2 != 60 || miss2 != 40 {
		t.Errorf("scenario2 failed: ok=%v hit=%d miss=%d", ok, hit2, miss2)
	}
	// 场景3：上游完全不返回缓存字段 → 兜底 miss=pt（全未命中，显示 0%）
	var hit3, miss3, pt3, ct3 int64
	ok = parseUsage([]byte(`{"usage":{"prompt_tokens":100,"completion_tokens":20}}`), &pt3, &ct3, &hit3, &miss3)
	if !ok || hit3 != 0 || miss3 != 100 {
		t.Errorf("scenario3 failed: ok=%v hit=%d miss=%d (want miss=100)", ok, hit3, miss3)
	}
	// 场景4：无 usage → false
	var hit4, miss4, pt4, ct4 int64
	ok = parseUsage([]byte(`{"id":"x"}`), &pt4, &ct4, &hit4, &miss4)
	if ok {
		t.Errorf("scenario4: expected false, got ok=%v", ok)
	}
}

// TestUpstreamTimeoutForUp 验证 per-upstream 超时优先级：
// 上游自配 timeout（秒）优先，未配时回退全局 retry.upstream_timeout，再回退内置 60s。
func TestUpstreamTimeoutForUp(t *testing.T) {
	if got := upstreamTimeoutForUp(nil, nil); got != 60*time.Second {
		t.Errorf("nil/nil = %v, want 60s", got)
	}
	cfg := &config.Config{}
	cfg.Retry.UpstreamTimeout = 120
	if got := upstreamTimeoutForUp(nil, cfg); got != 120*time.Second {
		t.Errorf("nil/global120 = %v, want 120s", got)
	}
	up := &config.Upstream{Timeout: 300}
	if got := upstreamTimeoutForUp(up, cfg); got != 300*time.Second {
		t.Errorf("up300 = %v, want 300s", got)
	}
	// 上游配置 0 = 跟随全局
	up0 := &config.Upstream{Timeout: 0}
	if got := upstreamTimeoutForUp(up0, cfg); got != 120*time.Second {
		t.Errorf("up0/global120 = %v, want 120s", got)
	}
}

// TestForwardOnce_4xxRecordsErrorDetail 验证：上游返回非可重试 4xx 时，
// 请求日志中 error_detail 字段记录了上游的 error.message，便于请求日志页排查。
// 对应场景：WorkBuddy 等客户端按窗口自动填 max_completion_tokens=384000，
// 上游 deepseek-v4-flash 最多支持 262144 → 返回 400 且不可重试。
func TestForwardOnce_4xxRecordsErrorDetail(t *testing.T) {
	// 模拟上游返回 400 不可重试（含 OpenAI 风格 error.message）
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"max_completion_tokens is too large: 384000. This model supports at most 262144 completion tokens.","type":"BadRequestError","code":400}}`))
	}))
	defer upstream.Close()

	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rec := store.NewRecorder(s)

	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{Name: "up", BaseURL: upstream.URL, APIKey: "sk-test", Priority: 10,
				Models:       []string{"deepseek-v4-flash"},
				ModelMapping: map[string]string{"deepseek-v4-flash": "deepseek-ai/DeepSeek-V4-Flash"},
			},
		},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules: []config.Rule{
				{Model: "deepseek-v4-flash", Upstreams: []string{"up"}, Strategy: "primary_backup"},
			},
		},
		Retry: config.Retry{}, // 空配置 → 所有 4xx 都不可重试
	}
	h := New(cfg, router.New(cfg), health.New(cfg))
	h.SetRecorder(rec)

	body := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:54321"
	recw := httptest.NewRecorder()
	h.ChatCompletions(recw, req)
	rec.Close() // flush

	// 网关应返回 400（透传上游）
	if recw.Code != http.StatusBadRequest {
		t.Fatalf("gateway status=%d body=%s, want 400", recw.Code, recw.Body.String())
	}

	// 请求日志中应有记录，且 error_detail 包含上游报错摘要
	// 诊断：打印表结构
	cols, _ := s.DB().Query("PRAGMA table_info(request_log)")
	for cols.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dfltValue sql.NullString
		cols.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk)
		t.Logf("column: %s (%s)", name, ctype)
	}
	cols.Close()
	rows, err := s.DB().Query(`SELECT status, error_detail FROM request_log`)
	if err != nil {
		t.Fatalf("query request_log: %v", err)
	}
	defer rows.Close()

	found := false
	for rows.Next() {
		var status int
		var errorDetail string
		if err := rows.Scan(&status, &errorDetail); err != nil {
			t.Fatal(err)
		}
		if status != http.StatusBadRequest {
			continue
		}
		found = true
		if !strings.Contains(errorDetail, "message=max_completion_tokens is too large") {
			t.Errorf("error_detail = %q, want to contain message= max_completion_tokens is too large", errorDetail)
		}
	}
	if !found {
		t.Fatal("no request_log record with status=400")
	}
}
