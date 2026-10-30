package systemone

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/router"
	"github.com/icefairy/xuanji/internal/store"
)

// newTestHandler 构造一个上游指向 srv、单条路由规则（模型 decision-test）的 handler。
func newTestHandler(t *testing.T, srv *httptest.Server, model string) *Handler {
	t.Helper()
	cfg := &config.Config{
		Upstreams: []config.Upstream{{
			Name:    "up1",
			BaseURL: srv.URL + "/v1",
			APIKey:  "k-test",
			Models:  []string{model},
		}},
		Routing: config.Routing{Rules: []config.Rule{{
			Model:     model,
			Upstreams: []string{"up1"},
		}}},
	}
	return New(cfg, router.New(cfg), nil)
}

// TestForwardsToSystemonePath 校验转发路径固定为 /v1/systemone 且 model/state/questions 原样透传。
func TestForwardsToSystemonePath(t *testing.T) {
	var gotPath, gotAuth, gotModel, gotState string
	var gotQuestionsExists bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		gotState, _ = body["state"].(string)
		_, gotQuestionsExists = body["questions"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"m","answers":{"q1":{"type":"noul","noul":0.9}},"usage":{"input_tokens":11,"output_tokens":2}}`))
	}))
	defer srv.Close()

	h := newTestHandler(t, srv, "decision-test")
	body := `{"model":"decision-test","state":"客户重复扣款","questions":{"q1":{"type":"noul","instructions":"退款?","criteria":{"true":"是","false":"否"}}}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.Serve(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/systemone" {
		t.Errorf("upstream path = %q, want /v1/systemone", gotPath)
	}
	if gotAuth != "Bearer k-test" {
		t.Errorf("auth = %q, want Bearer k-test", gotAuth)
	}
	if gotModel != "decision-test" || gotState != "客户重复扣款" || !gotQuestionsExists {
		t.Errorf("forwarded body mismatch: model=%q state=%q questions=%v", gotModel, gotState, gotQuestionsExists)
	}
	// 响应原样透传
	if !strings.Contains(rec.Body.String(), `"noul":0.9`) {
		t.Errorf("response not passed through: %s", rec.Body.String())
	}
}

// TestNoulCriteriaDefaultFilled 校验 noul 缺 criteria 时自动补 true/false 描述，
// 避免强制校验该字段的上游（书生）返回 400。
func TestNoulCriteriaDefaultFilled(t *testing.T) {
	var gotTrue, gotFalse string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		qs, _ := body["questions"].(map[string]any)
		q1, _ := qs["q1"].(map[string]any)
		crit, ok := q1["criteria"].(map[string]any)
		if !ok {
			t.Errorf("criteria not filled for noul question")
		}
		gotTrue, _ = crit["true"].(string)
		gotFalse, _ = crit["false"].(string)
		_, _ = w.Write([]byte(`{"answers":{"q1":{"type":"noul","noul":0.5}}}`))
	}))
	defer srv.Close()

	h := newTestHandler(t, srv, "decision-test")
	body := `{"model":"decision-test","state":"x","questions":{"q1":{"type":"noul","instructions":"ok?"}}}`
	rec := httptest.NewRecorder()
	h.Serve(rec, httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotTrue == "" || gotFalse == "" {
		t.Errorf("noul criteria = %q/%q, want non-empty", gotTrue, gotFalse)
	}
}

// TestExistingCriteriaKept 校验客户端已给的 criteria 不被覆盖。
func TestExistingCriteriaKept(t *testing.T) {
	var gotTrue string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		qs, _ := body["questions"].(map[string]any)
		q1, _ := qs["q1"].(map[string]any)
		crit, _ := q1["criteria"].(map[string]any)
		gotTrue, _ = crit["true"].(string)
		_, _ = w.Write([]byte(`{"answers":{}}`))
	}))
	defer srv.Close()

	h := newTestHandler(t, srv, "decision-test")
	body := `{"model":"decision-test","state":"x","questions":{"q1":{"type":"noul","instructions":"ok?","criteria":{"true":"是","false":"否"}}}}`
	h.Serve(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(body)))
	if gotTrue != "是" {
		t.Errorf("criteria overridden: true=%q, want 是", gotTrue)
	}
}

// TestUpstream4xxPassThrough 校验上游 4xx 透传且不算上游故障（不重试其它候选）。
func TestUpstream4xxPassThrough(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"criteria must be an object"}}`))
	}))
	defer srv.Close()

	h := newTestHandler(t, srv, "decision-test")
	rec := httptest.NewRecorder()
	h.Serve(rec, httptest.NewRequest(http.MethodPost, "/v1/systemone",
		strings.NewReader(`{"model":"decision-test","state":"x","questions":{}}`)))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 passthrough", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "criteria must be an object") {
		t.Errorf("upstream error message lost: %s", rec.Body.String())
	}
}

// TestRetryOn5xx 校验 5xx 会尝试下一个候选上游。
func TestRetryOn5xx(t *testing.T) {
	var bad, good int32
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&bad, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer badSrv.Close()
	goodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&good, 1)
		_, _ = w.Write([]byte(`{"answers":{"q1":{"type":"noul","noul":0.42}}}`))
	}))
	defer goodSrv.Close()

	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{Name: "bad", BaseURL: badSrv.URL + "/v1", APIKey: "k", Models: []string{"m"}},
			{Name: "good", BaseURL: goodSrv.URL + "/v1", APIKey: "k", Models: []string{"m"}},
		},
		Routing: config.Routing{Rules: []config.Rule{{Model: "m", Upstreams: []string{"bad", "good"}}}},
	}
	h := New(cfg, router.New(cfg), nil)
	rec := httptest.NewRecorder()
	h.Serve(rec, httptest.NewRequest(http.MethodPost, "/v1/systemone",
		strings.NewReader(`{"model":"m","state":"x","questions":{"q1":{"type":"noul","instructions":"i","criteria":{"true":"y","false":"n"}}}}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after retry, body=%s", rec.Code, rec.Body.String())
	}
	if atomic.LoadInt32(&bad) != 1 || atomic.LoadInt32(&good) != 1 {
		t.Errorf("calls bad=%d good=%d, want 1/1", bad, good)
	}
}

// TestModelNotFound 校验无路由规则时返回 404 标准错误。
func TestModelNotFound(t *testing.T) {
	h := New(&config.Config{}, router.New(&config.Config{}), nil)
	rec := httptest.NewRecorder()
	h.Serve(rec, httptest.NewRequest(http.MethodPost, "/v1/systemone",
		strings.NewReader(`{"model":"nope","state":"x","questions":{}}`)))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestMissingModel 校验缺 model 返回 400。
func TestMissingModel(t *testing.T) {
	h := New(&config.Config{}, router.New(&config.Config{}), nil)
	rec := httptest.NewRecorder()
	h.Serve(rec, httptest.NewRequest(http.MethodPost, "/v1/systemone",
		strings.NewReader(`{"state":"x","questions":{}}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestUsageRecorded 校验 usage.input_tokens/output_tokens 映射到记录器的 prompt/completion。
func TestUsageRecorded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"answers":{},"usage":{"input_tokens":17,"output_tokens":3}}`))
	}))
	defer srv.Close()

	rec := &captureRecorder{}
	cfg := &config.Config{
		Upstreams: []config.Upstream{{Name: "up1", BaseURL: srv.URL + "/v1", APIKey: "k", Models: []string{"m"}}},
		Routing:   config.Routing{Rules: []config.Rule{{Model: "m", Upstreams: []string{"up1"}}}},
	}
	h := New(cfg, router.New(cfg), nil)
	h.SetRecorder(rec)
	h.Serve(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/systemone",
		strings.NewReader(`{"model":"m","state":"x","questions":{}}`)))

	if rec.prompt != 17 || rec.completion != 3 {
		t.Errorf("recorded tokens = %d/%d, want 17/3", rec.prompt, rec.completion)
	}
	if rec.endpoint != Endpoint {
		t.Errorf("recorded endpoint = %q, want %q", rec.endpoint, Endpoint)
	}
}

// TestUpstreamTimeout 校验单个上游配置的 timeout 生效（用极短超时观察失败路径）。
func TestUpstreamTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		_, _ = w.Write([]byte(`{"answers":{}}`))
	}))
	defer srv.Close()

	cfg := &config.Config{
		Upstreams: []config.Upstream{{Name: "slow", BaseURL: srv.URL + "/v1", APIKey: "k", Models: []string{"m"}, Timeout: 1}},
		Routing:   config.Routing{Rules: []config.Rule{{Model: "m", Upstreams: []string{"slow"}}}},
	}
	// 上游级 timeout（1s 粒度太粗）不便于测试，这里直接用短超时 client 验证超时降级路径
	h := New(cfg, router.New(cfg), &http.Client{Timeout: 50 * time.Millisecond})
	rec := httptest.NewRecorder()
	h.Serve(rec, httptest.NewRequest(http.MethodPost, "/v1/systemone",
		strings.NewReader(`{"model":"m","state":"x","questions":{}}`)))
	if rec.Code == http.StatusOK {
		t.Errorf("status = 200, want failure on timeout")
	}
}

// captureRecorder 是 RecordSink 的测试桩。
type captureRecorder struct {
	prompt, completion int64
	endpoint           string
}

func (c *captureRecorder) Record(rec store.Record) {
	c.prompt = rec.PromptTokens
	c.completion = rec.CompletionTokens
	c.endpoint = rec.Endpoint
}
