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
	"github.com/tidwall/gjson"
)

// healthCfg 构造一个含 model 路由到指定上游的配置。
func healthCfg(upstreams []config.Upstream, model string, names []string) *config.Config {
	return &config.Config{
		Upstreams: upstreams,
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules: []config.Rule{
				{Model: model, Upstreams: names, Strategy: "primary_backup"},
			},
		},
		Retry: config.Retry{
			MaxRetries:    3,
			RetryStatuses: []int{429, 500, 502, 503, 504},
		},
	}
}

func TestChatCompletions_FiltersDeadUpstream(t *testing.T) {
	var deadCalled atomic.Bool
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"live","object":"chat.completion","choices":[]}`)
	}))
	defer live.Close()
	deadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		deadCalled.Store(true)
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"dead","object":"chat.completion","choices":[]}`)
	}))
	defer deadSrv.Close()

	cfg := healthCfg([]config.Upstream{
		{Name: "dead", BaseURL: deadSrv.URL, APIKey: "k", Priority: 10},
		{Name: "live", BaseURL: live.URL, APIKey: "k", Priority: 20},
	}, "m", []string{"dead", "live"})

	hc := health.New(cfg)
	for i := 0; i < 5; i++ {
		hc.MarkFailure("dead")
	}
	defer hc.Close()

	h := New(cfg, router.New(cfg), hc)
	rec := doChat(t, h, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if deadCalled.Load() {
		t.Error("dead upstream should not be called")
	}
	if got := gjson.Get(rec.Body.String(), "id").String(); got != "live" {
		t.Errorf("response id = %q, want live", got)
	}
}

func TestChatCompletions_FailoverOn5xx(t *testing.T) {
	var up1Hits, up2Hits atomic.Int32
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		up1Hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"message":"boom","type":"server_error"}}`)
	}))
	defer srv1.Close()
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		up2Hits.Add(1)
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"ok2","object":"chat.completion","choices":[]}`)
	}))
	defer srv2.Close()

	cfg := healthCfg([]config.Upstream{
		{Name: "up1", BaseURL: srv1.URL, APIKey: "k", Priority: 10},
		{Name: "up2", BaseURL: srv2.URL, APIKey: "k", Priority: 20},
	}, "m", []string{"up1", "up2"})

	hc := health.New(cfg)
	defer hc.Close()
	h := New(cfg, router.New(cfg), hc)

	rec := doChat(t, h, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := gjson.Get(rec.Body.String(), "id").String(); got != "ok2" {
		t.Errorf("response id = %q, want ok2 (failover)", got)
	}
	if up1Hits.Load() != 1 || up2Hits.Load() != 1 {
		t.Errorf("hits = up1:%d up2:%d, want 1/1", up1Hits.Load(), up2Hits.Load())
	}
	if got := hc.Status("up1"); got != health.StateDegraded {
		t.Errorf("up1 status = %q, want degraded after one forward failure", got)
	}
}

func TestChatCompletions_FailoverOn429(t *testing.T) {
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"quota","type":"rate_limit_error"}}`)
	}))
	defer srv1.Close()
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"ok2","object":"chat.completion","choices":[]}`)
	}))
	defer srv2.Close()

	cfg := healthCfg([]config.Upstream{
		{Name: "up1", BaseURL: srv1.URL, APIKey: "k", Priority: 10},
		{Name: "up2", BaseURL: srv2.URL, APIKey: "k", Priority: 20},
	}, "m", []string{"up1", "up2"})

	hc := health.New(cfg)
	defer hc.Close()
	h := New(cfg, router.New(cfg), hc)

	rec := doChat(t, h, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := gjson.Get(rec.Body.String(), "id").String(); got != "ok2" {
		t.Errorf("response id = %q, want ok2 (failover on 429)", got)
	}
}

func TestChatCompletions_FailoverOnConnectionError(t *testing.T) {
	deadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	deadSrv.Close() // 连接失败

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"ok2","object":"chat.completion","choices":[]}`)
	}))
	defer srv2.Close()

	cfg := healthCfg([]config.Upstream{
		{Name: "up1", BaseURL: deadSrv.URL, APIKey: "k", Priority: 10},
		{Name: "up2", BaseURL: srv2.URL, APIKey: "k", Priority: 20},
	}, "m", []string{"up1", "up2"})

	hc := health.New(cfg)
	defer hc.Close()
	h := New(cfg, router.New(cfg), hc)

	rec := doChat(t, h, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := gjson.Get(rec.Body.String(), "id").String(); got != "ok2" {
		t.Errorf("response id = %q, want ok2 (failover on connect error)", got)
	}
}

func TestChatCompletions_AllDeadFallsBackToFirst(t *testing.T) {
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"first","object":"chat.completion","choices":[]}`)
	}))
	defer srv1.Close()
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"second","object":"chat.completion","choices":[]}`)
	}))
	defer srv2.Close()

	cfg := healthCfg([]config.Upstream{
		{Name: "up1", BaseURL: srv1.URL, APIKey: "k", Priority: 10},
		{Name: "up2", BaseURL: srv2.URL, APIKey: "k", Priority: 20},
	}, "m", []string{"up1", "up2"})

	hc := health.New(cfg)
	for _, name := range []string{"up1", "up2"} {
		for i := 0; i < 5; i++ {
			hc.MarkFailure(name)
		}
	}
	defer hc.Close()

	h := New(cfg, router.New(cfg), hc)
	rec := doChat(t, h, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// 健康列表为空，回退到全部上游里第一个（up1）
	if got := gjson.Get(rec.Body.String(), "id").String(); got != "first" {
		t.Errorf("response id = %q, want first (fallback to first upstream)", got)
	}
}

func TestChatCompletions_StreamFailoverOn5xx(t *testing.T) {
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"message":"boom","type":"server_error"}}`)
	}))
	defer srv1.Close()
	const sse = "data: {\"id\":\"s2\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n"
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, sse)
	}))
	defer srv2.Close()

	cfg := healthCfg([]config.Upstream{
		{Name: "up1", BaseURL: srv1.URL, APIKey: "k", Priority: 10},
		{Name: "up2", BaseURL: srv2.URL, APIKey: "k", Priority: 20},
	}, "m", []string{"up1", "up2"})

	hc := health.New(cfg)
	defer hc.Close()
	h := New(cfg, router.New(cfg), hc)

	rec := doChat(t, h, `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := rec.Body.String(); got != sse {
		t.Errorf("SSE body = %q, want sse from up2 (stream failover)", got)
	}
}

func TestChatCompletions_NoHealthCheckerKeepsOldBehavior(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"plain","object":"chat.completion","choices":[]}`)
	}))
	defer srv.Close()

	cfg := healthCfg([]config.Upstream{
		{Name: "up", BaseURL: srv.URL, APIKey: "k"},
	}, "m", []string{"up"})

	h := New(cfg, router.New(cfg), nil)
	rec := doChat(t, h, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := gjson.Get(rec.Body.String(), "id").String(); got != "plain" {
		t.Errorf("response id = %q, want plain", got)
	}
}

func TestChatCompletions_ModelMappingAppliedPerCandidate(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(data)
		gotModel = gjson.GetBytes(data, "model").String()
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"mapped","object":"chat.completion","choices":[]}`)
	}))
	defer srv.Close()

	cfg := healthCfg([]config.Upstream{
		{Name: "up", BaseURL: srv.URL, APIKey: "k",
			ModelMapping: map[string]string{"m": "mapped-model"}},
	}, "m", []string{"up"})

	h := New(cfg, router.New(cfg), nil)
	rec := doChat(t, h, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if gotModel != "mapped-model" {
		t.Errorf("upstream received model = %q, want mapped-model", gotModel)
	}
	if !strings.Contains(rec.Body.String(), "mapped") {
		t.Errorf("response body = %q, want passthrough", rec.Body.String())
	}
}
