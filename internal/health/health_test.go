package health

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/icefairy/xuanji/internal/config"
)

func init() {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// testCfg 构造一个含指定上游的配置；未给上游时使用一个占位上游。
func testCfg(upstreams ...config.Upstream) *config.Config {
	if len(upstreams) == 0 {
		upstreams = []config.Upstream{{Name: "up", BaseURL: "http://unused", APIKey: "k"}}
	}
	return &config.Config{Upstreams: upstreams}
}

// startModelsServer 启动一个 /models 端点，是否返回失败由 fail 原子标志控制。
// 兼容 /models 与 /v1/models 两种探测路径（base_url 不带 /v1 时探测 /v1/models）。
func startModelsServer(t *testing.T, fail *atomic.Bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" && r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /models or /v1/models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization = %q, want Bearer sk-test", got)
		}
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
}

// TestPingPathVariants 验证探测路径拼接：base_url 不带 /v1 → 拼 /v1/models；
// 带 /v1 → 拼 /models（两者最终都打到 /v1/models 路径）；Ollama → /api/tags。
func TestPingPathVariants(t *testing.T) {
	cases := []struct {
		name   string
		suffix string // base_url 末尾附加，模拟配置了 /v1 的地址
		upType string
		want   string
	}{
		{name: "不带v1", want: "/v1/models"},
		{name: "带v1", suffix: "/v1", want: "/v1/models"},
		{name: "ollama", upType: "ollama", want: "/api/tags"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()
			up := config.Upstream{Name: "x", BaseURL: srv.URL + c.suffix, APIKey: "sk-test", Type: c.upType}

			ck := New(testCfg(up))
			defer ck.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			// 直接触发一次探测，等待异步 goroutine 写入 gotPath
			ck.checkOnce(ctx, &upstreamState{up: &up, timeout: time.Second})
			if gotPath != c.want {
				t.Errorf("probe path = %q, want %q", gotPath, c.want)
			}
		})
	}
}

func TestStateTransitions_HealthyToDegradedToDeadAndRecover(t *testing.T) {
	var fail atomic.Bool
	srv := startModelsServer(t, &fail)
	defer srv.Close()

	c := New(testCfg(config.Upstream{
		Name:    "up",
		BaseURL: srv.URL,
		APIKey:  "sk-test",
		HealthCheck: &config.HealthCheck{
			Interval: config.Duration(time.Second),
			Timeout:  config.Duration(time.Second),
		},
	}))
	defer c.Close()

	ctx := context.Background()
	if got := c.Status("up"); got != StateHealthy {
		t.Fatalf("initial status = %q, want healthy", got)
	}

	// 成功一次保持 healthy
	c.checkOnce(ctx, c.states["up"])
	if got := c.Status("up"); got != StateHealthy {
		t.Errorf("after ok status = %q, want healthy", got)
	}

	fail.Store(true)
	// 第 1 次失败：仍 healthy
	c.checkOnce(ctx, c.states["up"])
	if got := c.Status("up"); got != StateHealthy {
		t.Errorf("after 1 fail status = %q, want healthy", got)
	}
	// 第 2 次失败：degraded
	c.checkOnce(ctx, c.states["up"])
	if got := c.Status("up"); got != StateDegraded {
		t.Errorf("after 2 fails status = %q, want degraded", got)
	}
	// 第 3、4 次失败：仍 degraded
	c.checkOnce(ctx, c.states["up"])
	c.checkOnce(ctx, c.states["up"])
	if got := c.Status("up"); got != StateDegraded {
		t.Errorf("after 4 fails status = %q, want degraded", got)
	}
	// 第 5 次失败：dead
	c.checkOnce(ctx, c.states["up"])
	if got := c.Status("up"); got != StateDead {
		t.Errorf("after 5 fails status = %q, want dead", got)
	}

	// 恢复：成功一次即回 healthy
	fail.Store(false)
	c.checkOnce(ctx, c.states["up"])
	if got := c.Status("up"); got != StateHealthy {
		t.Errorf("after recovery status = %q, want healthy", got)
	}
}

func TestStateTransitions_TimeoutDirectlyDead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(testCfg(config.Upstream{
		Name:    "up",
		BaseURL: srv.URL,
		APIKey:  "k",
		HealthCheck: &config.HealthCheck{
			Timeout: config.Duration(10 * time.Millisecond),
		},
	}))
	defer c.Close()

	c.checkOnce(context.Background(), c.states["up"])
	if got := c.Status("up"); got != StateDead {
		t.Errorf("after timeout status = %q, want dead", got)
	}
}

func TestHealthyUpstreams_FiltersDeadKeepsOrder(t *testing.T) {
	var failA, failB, failC atomic.Bool
	srvA := startModelsServer(t, &failA)
	defer srvA.Close()
	srvB := startModelsServer(t, &failB)
	defer srvB.Close()
	srvC := startModelsServer(t, &failC)
	defer srvC.Close()

	upA := config.Upstream{Name: "a", BaseURL: srvA.URL, APIKey: "sk-test"}
	upB := config.Upstream{Name: "b", BaseURL: srvB.URL, APIKey: "sk-test"}
	upC := config.Upstream{Name: "c", BaseURL: srvC.URL, APIKey: "sk-test"}

	c := New(testCfg(upA, upB, upC))
	defer c.Close()
	ctx := context.Background()

	// upB -> dead（5 次失败）
	failB.Store(true)
	for i := 0; i < deadAfterFails; i++ {
		c.checkOnce(ctx, c.states["b"])
	}
	if got := c.Status("b"); got != StateDead {
		t.Fatalf("upstream b status = %q, want dead", got)
	}
	// upC -> degraded（2 次失败）
	failC.Store(true)
	c.checkOnce(ctx, c.states["c"])
	c.checkOnce(ctx, c.states["c"])
	if got := c.Status("c"); got != StateDegraded {
		t.Fatalf("upstream c status = %q, want degraded", got)
	}
	// upA -> healthy
	c.checkOnce(ctx, c.states["a"])
	if got := c.Status("a"); got != StateHealthy {
		t.Fatalf("upstream a status = %q, want healthy", got)
	}

	// 输入乱序，HealthyUpstreams 应排除 dead 的 b，保留 a（healthy）、c（degraded），且保持输入顺序
	in := []*config.Upstream{&upB, &upA, &upC}
	out := c.HealthyUpstreams(in)
	if len(out) != 2 || out[0].Name != "a" || out[1].Name != "c" {
		t.Errorf("HealthyUpstreams = %v, want [a c]", names(out))
	}
}

func TestHealthyUpstreams_UnknownUpstreamKept(t *testing.T) {
	c := New(testCfg(config.Upstream{Name: "a", BaseURL: "http://unused", APIKey: "k"}))
	defer c.Close()

	unknown := &config.Upstream{Name: "ghost", BaseURL: "http://x", APIKey: "k"}
	out := c.HealthyUpstreams([]*config.Upstream{unknown})
	if len(out) != 1 || out[0].Name != "ghost" {
		t.Errorf("HealthyUpstreams = %v, want [ghost] kept", names(out))
	}
}

func TestStatus_Unknown(t *testing.T) {
	c := New(testCfg())
	defer c.Close()
	if got := c.Status("ghost"); got != StateUnknown {
		t.Errorf("Status(ghost) = %q, want unknown", got)
	}
}

func TestMarkFailure_AdvancesState(t *testing.T) {
	c := New(testCfg(config.Upstream{Name: "up", BaseURL: "http://unused", APIKey: "k"}))
	defer c.Close()

	// 转发失败是真实请求失败，一次即降级为 degraded
	c.MarkFailure("up")
	if got := c.Status("up"); got != StateDegraded {
		t.Errorf("after 1 MarkFailure status = %q, want degraded", got)
	}
	c.MarkFailure("up")
	if got := c.Status("up"); got != StateDegraded {
		t.Errorf("after 2 MarkFailure status = %q, want degraded", got)
	}
	for i := 0; i < 3; i++ {
		c.MarkFailure("up")
	}
	if got := c.Status("up"); got != StateDead {
		t.Errorf("after 5 MarkFailure status = %q, want dead", got)
	}

	// 未知上游应忽略且不 panic
	c.MarkFailure("ghost")
}

func TestChecker_StartPeriodicCheckAndRecovery(t *testing.T) {
	var fail atomic.Bool
	srv := startModelsServer(t, &fail)
	defer srv.Close()

	c := New(testCfg(config.Upstream{
		Name:    "up",
		BaseURL: srv.URL,
		APIKey:  "sk-test",
		HealthCheck: &config.HealthCheck{
			Interval: config.Duration(20 * time.Millisecond),
			Timeout:  config.Duration(50 * time.Millisecond),
		},
	}))
	c.Start()
	defer c.Close()

	if got := c.Status("up"); got != StateHealthy {
		t.Fatalf("initial status = %q, want healthy", got)
	}

	fail.Store(true)
	eventually(t, 3*time.Second, func() bool { return c.Status("up") == StateDead })

	fail.Store(false)
	eventually(t, 3*time.Second, func() bool { return c.Status("up") == StateHealthy })
}

func eventually(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func names(ups []*config.Upstream) []string {
	out := make([]string, len(ups))
	for i, u := range ups {
		out[i] = u.Name
	}
	return out
}
