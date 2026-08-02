package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/health"
	"github.com/icefairy/xuanji/internal/store"
)

func init() {
	// 静默健康检查的日志，保持测试输出干净
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// testConfig 构造一个含 3 个上游、2 条路由规则的配置。
func testConfig() *config.Config {
	return &config.Config{
		Server: config.Server{Port: 8787, APIKeys: "sk-admin-1"},
		Upstreams: []config.Upstream{
			{Name: "硅基流动", Type: "openai", BaseURL: "http://a.local", APIKey: "sk-a",
				Tier: "payg", Priority: 5, Weight: 100, Models: []string{"deepseek-v4-flash", "bge-m3"}},
			{Name: "opencode_go", Type: "openai", BaseURL: "http://b.local", APIKey: "sk-b",
				Tier: "subscription", Priority: 2, Weight: 50, Models: []string{"gpt-5"}},
			{Name: "vllm-local", Type: "openai", BaseURL: "http://c.local", APIKey: "sk-c",
				Tier: "free", Priority: 1, Weight: 100, Models: []string{"qwen*"}},
		},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules: []config.Rule{
				// 第一条规则 strategy 留空，测试默认继承
				{Model: "deepseek-v4-flash", Upstreams: []string{"硅基流动", "opencode_go"}},
				{Model: "gpt-5", Upstreams: []string{"opencode_go"}, Strategy: "quota"},
			},
		},
	}
}

// newTestHandler 构建一个已绑定健康检查器的管理 Handler。
func newTestHandler(t *testing.T, cfg *config.Config) (*Handler, *health.Checker) {
	t.Helper()
	hc := health.New(cfg)
	return New(cfg, hc), hc
}

// decodeBody 解析响应体 JSON 到 out。
func decodeBody(t *testing.T, rr *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.Unmarshal(rr.Body.Bytes(), out); err != nil {
		t.Fatalf("decode response %q: %v", rr.Body.String(), err)
	}
}

// getStatus 请求 /admin/status 并返回解码结果。
func getStatus(t *testing.T, h *Handler) statusResponse {
	t.Helper()
	rr := httptest.NewRecorder()
	h.Status(rr, httptest.NewRequest(http.MethodGet, "/admin/status", nil))
	var out statusResponse
	decodeBody(t, rr, &out)
	return out
}

func TestStatus_Basic(t *testing.T) {
	cfg := testConfig()
	h, hc := newTestHandler(t, cfg)
	defer hc.Close()

	out := getStatus(t, h)
	if out.Service != "ok" {
		t.Errorf("service = %q, want ok", out.Service)
	}
	if out.UptimeSeconds < 0 {
		t.Errorf("uptime_seconds = %d, want >= 0", out.UptimeSeconds)
	}
	if out.Port != 8787 {
		t.Errorf("port = %d, want 8787", out.Port)
	}
	if out.UpstreamTotal != 3 {
		t.Errorf("upstream_total = %d, want 3", out.UpstreamTotal)
	}
	if out.RulesTotal != 2 {
		t.Errorf("rules_total = %d, want 2", out.RulesTotal)
	}
	if out.DefaultStrategy != "primary_backup" {
		t.Errorf("default_strategy = %q, want primary_backup", out.DefaultStrategy)
	}
}

func TestStatus_HealthCounts(t *testing.T) {
	cfg := testConfig()
	h, hc := newTestHandler(t, cfg)
	defer hc.Close()

	// 初始全 healthy
	out := getStatus(t, h)
	if out.UpstreamHealthy != 3 || out.UpstreamDegraded != 0 || out.UpstreamDead != 0 {
		t.Errorf("initial counts = h%d/d%d/dead%d, want 3/0/0",
			out.UpstreamHealthy, out.UpstreamDegraded, out.UpstreamDead)
	}

	// 硅基流动 -> degraded（1 次转发失败）
	hc.MarkFailure("硅基流动")
	// vllm-local -> dead（5 次转发失败）
	for i := 0; i < 5; i++ {
		hc.MarkFailure("vllm-local")
	}

	out = getStatus(t, h)
	if out.UpstreamHealthy != 1 {
		t.Errorf("upstream_healthy = %d, want 1", out.UpstreamHealthy)
	}
	if out.UpstreamDegraded != 1 {
		t.Errorf("upstream_degraded = %d, want 1", out.UpstreamDegraded)
	}
	if out.UpstreamDead != 1 {
		t.Errorf("upstream_dead = %d, want 1", out.UpstreamDead)
	}
}

func TestUpstreams_NoAPIKey(t *testing.T) {
	cfg := testConfig()
	// 故意带上显眼的 key 值，确认绝不泄漏
	cfg.Upstreams[0].APIKey = "sk-super-secret-a"
	h, hc := newTestHandler(t, cfg)
	defer hc.Close()

	rr := httptest.NewRecorder()
	h.Upstreams(rr, httptest.NewRequest(http.MethodGet, "/admin/upstreams", nil))

	body := rr.Body.String()
	if strings.Contains(body, "«redacted:sk-…»") {
		t.Errorf("response must not leak api_key value, got body: %s", body)
	}
}

func TestUpstreams_LatencyAndState(t *testing.T) {
	cfg := testConfig()
	h, hc := newTestHandler(t, cfg)
	defer hc.Close()

	hc.SetLatencyForTest("硅基流动", 442*time.Millisecond)
	hc.MarkFailure("opencode_go") // -> degraded

	rr := httptest.NewRecorder()
	h.Upstreams(rr, httptest.NewRequest(http.MethodGet, "/admin/upstreams", nil))

	var out []upstreamResponse
	decodeBody(t, rr, &out)
	if len(out) != 3 {
		t.Fatalf("len(out) = %d, want 3", len(out))
	}

	// 配置顺序：硅基流动、opencode_go、vllm-local
	if out[0].Name != "硅基流动" || out[1].Name != "opencode_go" || out[2].Name != "vllm-local" {
		t.Errorf("upstream order = [%s %s %s], want [硅基流动 opencode_go vllm-local]",
			out[0].Name, out[1].Name, out[2].Name)
	}
	if out[0].LatencyMS != 442 {
		t.Errorf("硅基流动 latency_ms = %d, want 442", out[0].LatencyMS)
	}
	if out[0].State != string(health.StateHealthy) {
		t.Errorf("硅基流动 state = %q, want healthy", out[0].State)
	}
	if out[1].State != string(health.StateDegraded) {
		t.Errorf("opencode_go state = %q, want degraded", out[1].State)
	}
	if len(out[0].Models) != 2 || out[0].ModelCount != 2 {
		t.Errorf("硅基流动 models = %v, count = %d; want [deepseek-v4-flash bge-m3], 2",
			out[0].Models, out[0].ModelCount)
	}
}

func TestRules_StrategyInherit(t *testing.T) {
	cfg := testConfig()
	h, hc := newTestHandler(t, cfg)
	defer hc.Close()

	rr := httptest.NewRecorder()
	h.Rules(rr, httptest.NewRequest(http.MethodGet, "/admin/rules", nil))

	var out []ruleResponse
	decodeBody(t, rr, &out)
	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2", len(out))
	}

	// 第一条 rule.Strategy 为空，应继承默认 primary_backup
	if out[0].Strategy != "primary_backup" {
		t.Errorf("rules[0].strategy = %q, want inherited primary_backup", out[0].Strategy)
	}
	// 第二条显式 quota，保持不变
	if out[1].Strategy != "quota" {
		t.Errorf("rules[1].strategy = %q, want quota", out[1].Strategy)
	}
	if out[0].Model != "deepseek-v4-flash" || len(out[0].Upstreams) != 2 {
		t.Errorf("rules[0] = %+v, want model=deepseek-v4-flash upstreams=2", out[0])
	}
}

func TestConfig_Endpoints(t *testing.T) {
	cfg := testConfig() // APIKeys = "sk-admin-1"
	h, hc := newTestHandler(t, cfg)
	defer hc.Close()

	rr := httptest.NewRecorder()
	h.Config(rr, httptest.NewRequest(http.MethodGet, "/admin/config", nil))

	var out configResponse
	decodeBody(t, rr, &out)
	if len(out.Endpoints) == 0 {
		t.Fatal("endpoints is empty")
	}
	if !out.Server.APIKeysConfigured {
		t.Error("api_keys_configured = false, want true when APIKeys set")
	}
	if out.Server.Port != 8787 {
		t.Errorf("server.port = %d, want 8787", out.Server.Port)
	}
	if out.DefaultStrategy != "primary_backup" {
		t.Errorf("default_strategy = %q, want primary_backup", out.DefaultStrategy)
	}
	body := rr.Body.String()
	// api_keys_configured 是合法布尔字段，但绝不能出现密钥明文
	if strings.Contains(body, "sk-admin-1") {
		t.Errorf("config response must not leak api_key value, got body: %s", body)
	}

	// APIKeys 为空时 api_keys_configured 应为 false
	cfg2 := testConfig()
	cfg2.Server.APIKeys = ""
	h2, hc2 := newTestHandler(t, cfg2)
	defer hc2.Close()
	rr2 := httptest.NewRecorder()
	h2.Config(rr2, httptest.NewRequest(http.MethodGet, "/admin/config", nil))
	var out2 configResponse
	decodeBody(t, rr2, &out2)
	if out2.Server.APIKeysConfigured {
		t.Error("api_keys_configured = true, want false when APIKeys empty")
	}
}

func TestMetricsSummary_NoStore(t *testing.T) {
	cfg := testConfig()
	h, hc := newTestHandler(t, cfg)
	defer hc.Close()

	rr := httptest.NewRecorder()
	h.MetricsSummary(rr, httptest.NewRequest(http.MethodGet, "/admin/metrics/summary", nil))

	var out metricsSummaryResponse
	decodeBody(t, rr, &out)
	// store 为 nil，应返回零值
	if out.TotalRequests != 0 || out.TotalSuccesses != 0 || out.TotalTokens != 0 {
		t.Errorf("expected zero summary, got %+v", out)
	}
}

func TestMetricsUpstreams_NoStore(t *testing.T) {
	cfg := testConfig()
	h, hc := newTestHandler(t, cfg)
	defer hc.Close()

	rr := httptest.NewRecorder()
	h.MetricsUpstreams(rr, httptest.NewRequest(http.MethodGet, "/admin/metrics/upstreams", nil))

	var out []upstreamMetrics
	decodeBody(t, rr, &out)
	if out == nil {
		t.Error("expected empty array, got nil")
	}
	if len(out) != 0 {
		t.Errorf("expected empty array, got %+v", out)
	}
}

func TestMetricsHourly_NoStore(t *testing.T) {
	cfg := testConfig()
	h, hc := newTestHandler(t, cfg)
	defer hc.Close()

	rr := httptest.NewRecorder()
	h.MetricsHourly(rr, httptest.NewRequest(http.MethodGet, "/admin/metrics/hourly", nil))

	var out []hourlyBucket
	decodeBody(t, rr, &out)
	if out == nil {
		t.Error("expected empty array, got nil")
	}
	if len(out) != 0 {
		t.Errorf("expected empty array, got %+v", out)
	}
}

func TestGetAllConfig_WithStore(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()
	if err := s.SeedDefaults(); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}
	s.SetConfig("retry.max_retries", "5")

	cfg := testConfig()
	h, hc := newTestHandler(t, cfg)
	defer hc.Close()
	h.SetStore(s)

	rr := httptest.NewRecorder()
	h.GetAllConfig(rr, httptest.NewRequest(http.MethodGet, "/admin/config", nil))
	var out map[string]string
	decodeBody(t, rr, &out)
	if out["server.port"] != "8787" {
		t.Errorf("server.port = %q, want 8787", out["server.port"])
	}
	if out["retry.max_retries"] != "5" {
		t.Errorf("retry.max_retries = %q, want 5", out["retry.max_retries"])
	}
}

func TestGetAllConfig_NoStore(t *testing.T) {
	cfg := testConfig()
	h, hc := newTestHandler(t, cfg)
	defer hc.Close()

	rr := httptest.NewRecorder()
	h.GetAllConfig(rr, httptest.NewRequest(http.MethodGet, "/admin/config", nil))
	var out map[string]interface{}
	decodeBody(t, rr, &out)
	if out["error"] != "store not available" {
		t.Errorf("error = %v, want store not available", out["error"])
	}
}

func TestUpdateConfig(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()
	if err := s.SeedDefaults(); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}

	var reloaded int
	cfg := testConfig()
	h, hc := newTestHandler(t, cfg)
	defer hc.Close()
	h.SetStore(s)
	h.SetReload(func() error { reloaded++; return nil })

	rr := httptest.NewRecorder()
	body := strings.NewReader(`{"retry.max_retries":"7","server.port":"9000"}`)
	h.UpdateConfig(rr, httptest.NewRequest(http.MethodPut, "/admin/config", body))

	var out map[string]string
	decodeBody(t, rr, &out)
	if out["status"] != "ok" {
		t.Errorf("status = %q, want ok", out["status"])
	}
	if reloaded != 1 {
		t.Errorf("reload called %d times, want 1", reloaded)
	}
	if v, _ := s.GetConfig("retry.max_retries"); v != "7" {
		t.Errorf("retry.max_retries = %q, want 7", v)
	}
	if v, _ := s.GetConfig("server.port"); v != "9000" {
		t.Errorf("server.port = %q, want 9000", v)
	}
}
