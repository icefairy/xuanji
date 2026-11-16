package proxy

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/health"
	"github.com/icefairy/xuanji/internal/router"
	"github.com/icefairy/xuanji/internal/store"
)

// TestChatCompletions_Deterministic4xxDoesNotRetrySameUpstream 回归测试（2026-10-13 实测）：
// 单候选上游返回**确定性 4xx**（401 Invalid API Key）时，不应把同一上游重试 maxRetries 次。
//
// 现象（2026-10-13 日志）：
//
//	18:26:47 mimo 401 Invalid API Key → fails 1..10（10 条 mark_failed，state=dead）
//	18:28:01 mimo 401 Invalid API Key → fails 1..10
//	→ 2 个客户端请求刷出 **20 条** mark_failed + 20 条 trying next，上游瞬间被打成 dead
//
// 根因：重试循环末尾的 `i = -1` 重置（「网络可能恢复，从头再试」）只排除了 429 与
// vendor.ErrNoAccounts；401/403/400 等**确定性错误**仍会触发重置 → 单候选时反复重试
// 同一个上游（同一 API key），必然同样失败。与 2026-09-18 修的 ErrNoAccounts 放大器同源
// （当时注释即写明「单候选时 i=-1 重置会把同一上游重试 10 次（1 个请求刷出 10 条 mark_failed）」）。
func TestChatCompletions_Deterministic4xxDoesNotRetrySameUpstream(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid API Key","param":"Please provide valid API Key","code":"401","type":"invalid_key"}}`))
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
			Name: "mimo", BaseURL: up.URL, APIKey: "sk-bad", Weight: 100,
			Models: []string{"mimo-v2.5-tts"},
		}},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules: []config.Rule{
				{Model: "mimo-v2.5-tts", Upstreams: []string{"mimo"}, Strategy: "primary_backup"},
			},
		},
		// 与线上一致：401 在重试白名单内、max_retries=10（放大器的两个前提）
		Retry: config.Retry{MaxRetries: 10, RetryStatuses: []int{400, 401, 403, 404, 429, 451, 500, 502, 503, 504}},
	}
	hc := health.New(cfg)
	defer hc.Close()
	h := New(cfg, router.New(cfg), hc)
	h.SetRecorder(rec)

	doChat(t, h, `{"model":"mimo-v2.5-tts","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	rec.Close() // flush

	// 核心断言：确定性 4xx 只应尝试 1 次（重试同一 key 结果不变）
	if got := hits.Load(); got != 1 {
		t.Errorf("上游被请求 %d 次，want 1 —— 401 是确定性错误，"+
			"重试同一上游（同一 API key）必然同样失败；"+
			"2026-10-13 实测：2 个请求刷出 20 条 mark_failed，上游瞬间 dead", got)
	}
	// 健康计数不应被放大：1 次请求最多记 1 次失败（degraded），绝不能到 dead（deadAfterFails=5）
	if st := hc.Status("mimo"); st == health.StateDead {
		t.Errorf("health = dead，want 非 dead —— 单个请求不应把上游打成 dead（失败计数被放大）")
	}
}

// TestChatCompletions_Transient5xxStillRetriesSameUpstream 反向保护：
// 5xx 属**瞬时**错误（上游可能已恢复），必须保留「重置索引从头再试」的既有行为，
// 修复不得把确定性 4xx 的规则误用到 5xx 上。
func TestChatCompletions_Transient5xxStillRetriesSameUpstream(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream busy"}}`))
	}))
	defer up.Close()

	cfg := &config.Config{
		Upstreams: []config.Upstream{{
			Name: "up5xx", BaseURL: up.URL, APIKey: "k", Weight: 100,
			Models: []string{"m"},
		}},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules:           []config.Rule{{Model: "m", Upstreams: []string{"up5xx"}, Strategy: "primary_backup"}},
		},
		Retry: config.Retry{MaxRetries: 3, RetryStatuses: []int{429, 500, 502, 503, 504}},
	}
	h := New(cfg, router.New(cfg), nil)

	doChat(t, h, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":false}`)

	if got := hits.Load(); got <= 1 {
		t.Errorf("上游被请求 %d 次，want >1 —— 5xx 是瞬时错误，"+
			"应保留「重置索引重试同一上游」的既有行为（上游可能已恢复）", got)
	}
}
