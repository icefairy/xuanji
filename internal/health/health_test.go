package health

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/icefairy/xuanji/internal/config"
)

func init() {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// testCfg 构造一个含指定上游的配置；未给上游时使用一个占位上游。
// Enabled 统一强制为 true：健康探测只对启用中的上游进行，零值 Enabled=false
// 会被 checkOnce 跳过（见 TestDisabledUpstream_NoProbeAndUnknown），
// 这里保持既有测试「默认视为可探测」的语义。
func testCfg(upstreams ...config.Upstream) *config.Config {
	if len(upstreams) == 0 {
		upstreams = []config.Upstream{{Name: "up", BaseURL: "http://unused", APIKey: "k"}}
	}
	for i := range upstreams {
		upstreams[i].Enabled = true
	}
	return &config.Config{Upstreams: upstreams}
}

// startChatServer 启动一个 OpenAI 兼容探测端点：POST /v1/chat/completions（2xx=健康），
// 由 fail 原子标志控制返回 200/503。断言探测请求为 POST 且不携带 Authorization
// （无凭证探测，2026-08-24 统一）。
func startChatServer(t *testing.T, fail *atomic.Bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" && r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q, want /chat/completions or /v1/chat/completions", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want empty (无凭证探测)", got)
		}
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
}

// TestDisabledUpstream_NoProbeAndUnknown 验证禁用（Enabled=false）的上游被健康检查跳过：
// 不发起任何探测请求（命中计数保持零），状态置 unknown（不参与 healthy/degraded/dead 计数）；
// 重新启用后恢复正常探测（一次 checkOnce 即回 healthy 并产生一次真实探测）。
func TestDisabledUpstream_NoProbeAndUnknown(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Enabled 默认 false（零值）＝「禁用」上游；不经 testCfg（那里会强制启用），
	// 探测配置齐全，若未跳过会产生真实请求
	ck := New(&config.Config{Upstreams: []config.Upstream{{
		Name:         "disabled-up",
		BaseURL:      srv.URL,
		APIKey:       "sk-test",
		ModelMapping: map[string]string{"client": "server-model"},
		HealthCheck: &config.HealthCheck{
			Interval: config.Duration(10 * time.Millisecond),
			Timeout:  config.Duration(time.Second),
		},
	}}})
	defer ck.Close()

	ctx := context.Background()
	// 禁用状态连续多个周期：零命中、状态始终保持 unknown
	for i := 0; i < 5; i++ {
		ck.checkOnce(ctx, ck.states["disabled-up"])
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("disabled upstream got %d probe hits, want 0", got)
	}
	if got := ck.Status("disabled-up"); got != StateUnknown {
		t.Fatalf("disabled upstream status = %q, want unknown", got)
	}

	// 重新启用：下一次 checkOnce 走真实探测 → healthy，且命中计数 +1
	ck.mu.Lock()
	ck.states["disabled-up"].up.Enabled = true
	ck.mu.Unlock()
	ck.checkOnce(ctx, ck.states["disabled-up"])
	if got := hits.Load(); got != 1 {
		t.Fatalf("re-enabled upstream got %d probe hits, want 1", got)
	}
	if got := ck.Status("disabled-up"); got != StateHealthy {
		t.Fatalf("re-enabled upstream status = %q, want healthy", got)
	}
}

// TestPingPathVariants 验证探测路径拼接：OpenAI 兼容 → POST {base}/chat/completions
// （不带 /v1 时拼 /chat/completions，带 /v1 时拼 /v1/chat/completions）；Ollama → GET /api/tags。
func TestPingPathVariants(t *testing.T) {
	cases := []struct {
		name   string
		suffix string // base_url 末尾附加，模拟配置了 /v1 的地址
		upType string
		want   string
	}{
		{name: "openai带v1", suffix: "/v1", want: "/v1/chat/completions"},
		{name: "openai裸", suffix: "", want: "/chat/completions"},
		{name: "ollama", upType: "ollama", want: "/api/tags"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotPath, gotMethod string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotMethod = r.Method
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()
			up := config.Upstream{Name: "x", BaseURL: srv.URL + c.suffix, APIKey: "sk-test", Type: c.upType, Enabled: true}
			if c.upType != "ollama" {
				// openai 探测需要模型名（chatProbe 无模型名会直接返回失败不发请求）
				up.ModelMapping = map[string]string{"client": "server-model"}
			}
			ck := New(testCfg(up))
			defer ck.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			// 直接触发一次探测，等待异步 goroutine 写入 gotPath
			ck.checkOnce(ctx, &upstreamState{up: &up, timeout: time.Second})
			if gotPath != c.want {
				t.Errorf("probe path = %q, want %q", gotPath, c.want)
			}
			expMethod := http.MethodGet
			if c.upType != "ollama" {
				expMethod = http.MethodPost
			}
			if gotMethod != expMethod {
				t.Errorf("probe method = %q, want %q", gotMethod, expMethod)
			}
		})
	}
}

// TestChatProbe_OpenAIUpstream 验证 OpenAI 兼容上游（openai 类型）走 POST /chat/completions 探测：
// 模拟微信端点行为——GET /models 一律 400 "missing required parameter: model"，
// 但 chat 探测 2xx → healthy；探测请求不带 Authorization（无凭证模式）；
// 连续 chat 5xx 失败 → degraded；模型名用占位名 xuanji-probe（2026-08-26 起，
// 不再取 ModelMapping 真实模型：无鉴权上游下真实名会每 30s 真实推理一次）。
func TestChatProbe_OpenAIUpstream(t *testing.T) {
	var chatFail atomic.Bool
	var gotPath, gotModel, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			var req chatProbeReq
			_ = json.NewDecoder(r.Body).Decode(&req)
			gotModel = req.Model
			if chatFail.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"message":"internal error"}}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "x", "object": "chat.completion",
				"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "ok"}}},
			})
			return
		}
		// 模拟微信：对未知 GET（含 /models）一律 400
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"missing required parameter: model","code":400}}`))
	}))
	defer srv.Close()

	up := config.Upstream{
		Name:         "wechat-up",
		BaseURL:      srv.URL + "/v1",
		APIKey:       "sk-wx",
		ModelMapping: map[string]string{"deepseek-v4-flash": "Deepseek-v4-flash"},
	}
	ck := New(testCfg(up))
	defer ck.Close()
	st := ck.states[up.Name] // 用 Checker 内部 state，保证 checkOnce 与 Status 操作同一对象
	if st == nil {
		t.Fatalf("state for %q not found", up.Name)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// 成功路径：探测应打 POST /v1/chat/completions（而非 GET /models），带映射后真实模型名，
	// 且不携带 Authorization（无凭证探测）
	ck.checkOnce(ctx, st)
	if gotPath != "/v1/chat/completions" {
		t.Errorf("probe path = %q, want /v1/chat/completions", gotPath)
	}
	if gotModel != probeModelName {
		t.Errorf("probe model = %q, want %q (占位模型名，零推理消耗)", gotModel, probeModelName)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty (无凭证探测)", gotAuth)
	}
	if ck.Status("wechat-up") != StateHealthy {
		t.Errorf("status = %q, want healthy after successful chat probe", ck.Status("wechat-up"))
	}

	// 失败路径：chat 返回 500，连续两次失败应进入 degraded
	chatFail.Store(true)
	ck.checkOnce(ctx, st)
	ck.checkOnce(ctx, st)
	if ck.Status("wechat-up") != StateDegraded {
		t.Errorf("status = %q, want degraded after 2 failed chat probes", ck.Status("wechat-up"))
	}
}

// TestAuthRejectedIsHealthy 验证无凭证探活核心语义（2026-08-26 统一）：上游对探测请求返回
// 任意 4xx（400/401/403/404/405/429）均视为健康——请求已穿透到应用层被业务逻辑处理，
// 恰好证明网络通、端点存在、服务进程活。典型如纯 TTS 模型对 chat 探测返回 400
// "no chat template"：此前误判 dead，现在正确判健康。仅 5xx 判定失败。
func TestAuthRejectedIsHealthy(t *testing.T) {
	cases := []struct {
		name string
		code int
		want State
	}{
		{name: "401视为健康", code: http.StatusUnauthorized, want: StateHealthy},
		{name: "403视为健康", code: http.StatusForbidden, want: StateHealthy},
		{name: "400视为健康", code: http.StatusBadRequest, want: StateHealthy},
		{name: "404视为健康", code: http.StatusNotFound, want: StateHealthy},
		{name: "405视为健康", code: http.StatusMethodNotAllowed, want: StateHealthy},
		{name: "429视为健康", code: http.StatusTooManyRequests, want: StateHealthy},
		{name: "500仍失败", code: http.StatusInternalServerError, want: StateDegraded},
		{name: "502仍失败", code: http.StatusBadGateway, want: StateDegraded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(`{"error":{"message":"x"}}`))
			}))
			defer srv.Close()

			up := config.Upstream{Name: "auth-up", BaseURL: srv.URL + "/v1", APIKey: "sk-real"}
			ck := New(testCfg(up))
			defer ck.Close()
			st := ck.states[up.Name]
			if st == nil {
				t.Fatalf("state not found")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			// 连续两次达到 degraded 阈值，验证稳定判定
			ck.checkOnce(ctx, st)
			ck.checkOnce(ctx, st)
			if got := ck.Status("auth-up"); got != tc.want {
				t.Errorf("code %d: status = %q, want %q", tc.code, got, tc.want)
			}
		})
	}
}

func TestStateTransitions_HealthyToDegradedToDeadAndRecover(t *testing.T) {
	var fail atomic.Bool
	srv := startChatServer(t, &fail)
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
	srvA := startChatServer(t, &failA)
	defer srvA.Close()
	srvB := startChatServer(t, &failB)
	defer srvB.Close()
	srvC := startChatServer(t, &failC)
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

// TestPingFallback405ToEmbeddings 验证 chat 探测遇 404/405（chat 端点不存在，典型于
// 仅提供 embeddings 的网关如 Cloudflare Workers AI）时回退 POST /embeddings：
// 回退 2xx → 健康且取得 embeddings 端点延迟；回退失败（500）也保持 4xx 豁免的健康判定
// （2026-08-26 起，404/405 本身已是健康信号，不再误判 dead）。探测一律用占位模型名。
func TestPingFallback405ToEmbeddings(t *testing.T) {
	cases := []struct {
		name        string
		chatStatus  int  // POST /chat/completions 状态码
		embStatus   int  // POST /embeddings 状态码（0 表示服务端不应收到 POST）
		wantOK      bool // 最终探测结果
		wantEmbFall bool // 是否发生 embeddings 回退请求
	}{
		{
			name:        "405+emb200→健康且取emb延迟",
			chatStatus:  http.StatusMethodNotAllowed,
			embStatus:   http.StatusOK,
			wantOK:      true,
			wantEmbFall: true,
		},
		{
			name:        "405+emb500→仍健康（4xx豁免）",
			chatStatus:  http.StatusMethodNotAllowed,
			embStatus:   http.StatusInternalServerError,
			wantOK:      true,
			wantEmbFall: true,
		},
		{
			name:        "404+emb200→健康",
			chatStatus:  http.StatusNotFound,
			embStatus:   http.StatusOK,
			wantOK:      true,
			wantEmbFall: true,
		},
		{
			name:       "500不触发回退→失败",
			chatStatus: http.StatusInternalServerError,
			embStatus:  0,
			wantOK:     false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var embReqMethod, embReqCT, embReqAuth string
			var embReqBody []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/chat/completions":
					if got := r.Header.Get("Authorization"); got != "" {
						t.Errorf("Authorization = %q, want empty (无凭证探测)", got)
					}
					w.WriteHeader(c.chatStatus)
					_, _ = io.WriteString(w, `{"code":7001,"message":"chat endpoint not supported"}`)
				case "/embeddings":
					if c.embStatus == 0 {
						t.Errorf("unexpected POST /embeddings, path = %q", r.URL.Path)
					}
					embReqMethod = r.Method
					embReqCT = r.Header.Get("Content-Type")
					embReqAuth = r.Header.Get("Authorization")
					embReqBody, _ = io.ReadAll(r.Body)
					w.WriteHeader(c.embStatus)
				default:
					t.Errorf("unexpected path %q", r.URL.Path)
				}
			}))
			defer srv.Close()

			up := config.Upstream{Name: "cfcdn", BaseURL: srv.URL, APIKey: "sk-test"}
			ck := New(testCfg(up))
			defer ck.Close()

			out := ck.ping(context.Background(), &upstreamState{up: &up, timeout: 2 * time.Second})
			if out.ok != c.wantOK {
				t.Errorf("ok = %v, want %v (out=%+v)", out.ok, c.wantOK, out)
			}
			if (embReqMethod != "") != c.wantEmbFall {
				t.Errorf("embeddings fallback happened = %v, want %v", embReqMethod != "", c.wantEmbFall)
			}
			if c.embStatus != 0 {
				// 验证回退请求的姿势：POST + JSON + 无凭证（2026-08-24 统一）
				if embReqMethod != http.MethodPost {
					t.Errorf("embeddings method = %q, want POST", embReqMethod)
				}
				if embReqCT != "application/json" {
					t.Errorf("Content-Type = %q, want application/json", embReqCT)
				}
				if embReqAuth != "" {
					t.Errorf("Authorization = %q, want empty (无凭证探测)", embReqAuth)
				}
				// 验证请求体：占位模型名 + input=ping
				var payload struct {
					Model string `json:"model"`
					Input string `json:"input"`
				}
				if err := json.Unmarshal(embReqBody, &payload); err != nil {
					t.Fatalf("decode embeddings body: %v", err)
				}
				if payload.Model != probeModelName || payload.Input != "ping" {
					t.Errorf("embeddings payload = %+v, want {model:%s input:ping}", payload, probeModelName)
				}
			}
		})
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
	srv := startChatServer(t, &fail)
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

// TestChatProbe_429TreatedAsHealthy 验证 chat 探测对 429（限流）视为健康：
// 部分上游（如 tokenrhythm.studio / 基元律动）对无凭证/高频探测直接返回 429，
// 但真实带 key 请求是 200 成功，故不应误判 dead。仅 chatProbe 容错 429，
// ollama/embeddings 探测不受影响（见 authRejected）。
func TestChatProbe_429TreatedAsHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limit"}}`)
	}))
	defer srv.Close()

	up := config.Upstream{Name: "limit-up", BaseURL: srv.URL + "/v1", APIKey: "sk-real"}
	ck := New(testCfg(up))
	defer ck.Close()
	st := ck.states[up.Name]
	if st == nil {
		t.Fatalf("state not found")
	}

	// 1) 直接断言探测结果：429 → ok=true（视为健康）
	out := ck.ping(context.Background(), &upstreamState{up: &up, timeout: 2 * time.Second})
	if !out.ok {
		t.Fatalf("429 probe ok = false, want true (out=%+v)", out)
	}

	// 2) 状态机视角：连续两次探测仍保持 healthy（不降级为 degraded/dead）
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ck.checkOnce(ctx, st)
	ck.checkOnce(ctx, st)
	if got := ck.Status("limit-up"); got != StateHealthy {
		t.Errorf("status after 429 probes = %q, want healthy", got)
	}
}

// TestKindProbes 按 Upstream.Kind 分派探测端点（2026-08-26 新增）：
//
//	chat → /chat/completions, emb → /embeddings, rerank → /rerank,
//	tts → /audio/speech, asr → /audio/transcriptions, image → /images/generations
//
// 验证各 kind 请求打对路径、body 携带占位模型名 probeModelName、且对应用层业务拒绝
// （4xx，如 TTS 模型无 chat template 返回 400 "no chat template"）判健康不误判 dead。
func TestKindProbes(t *testing.T) {
	cases := []struct {
		name     string
		kind     string // 上游 kind；"" 为默认 chat
		wantPath string
		// JSON 请求体验证（asr 为 multipart 由 handler 内另行断言）；nil 跳过
		verifyBody func(t *testing.T, body []byte)
	}{
		{
			name: "chat默认", kind: "", wantPath: "/chat/completions",
			verifyBody: func(t *testing.T, body []byte) {
				var p chatProbeReq
				mustDecode(t, body, &p)
				if p.Model != probeModelName || p.MaxTokens != 1 || len(p.Messages) != 1 {
					t.Errorf("chat payload = %+v, want model=%s max_tokens=1", p, probeModelName)
				}
			},
		},
		{
			name: "emb", kind: "emb", wantPath: "/embeddings",
			verifyBody: func(t *testing.T, body []byte) {
				var p embeddingsProbeReq
				mustDecode(t, body, &p)
				if p.Model != probeModelName || p.Input != "ping" {
					t.Errorf("emb payload = %+v", p)
				}
			},
		},
		{
			name: "rerank", kind: "rerank", wantPath: "/rerank",
			verifyBody: func(t *testing.T, body []byte) {
				var p rerankProbeReq
				mustDecode(t, body, &p)
				if p.Model != probeModelName || p.Query != "ping" || len(p.Documents) == 0 {
					t.Errorf("rerank payload = %+v", p)
				}
			},
		},
		{
			name: "tts", kind: "tts", wantPath: "/audio/speech",
			verifyBody: func(t *testing.T, body []byte) {
				var p ttsProbeReq
				mustDecode(t, body, &p)
				if p.Model != probeModelName || p.Input != "ping" {
					t.Errorf("tts payload = %+v", p)
				}
			},
		},
		{
			name: "image", kind: "image", wantPath: "/images/generations",
			verifyBody: func(t *testing.T, body []byte) {
				var p imageProbeReq
				mustDecode(t, body, &p)
				if p.Model != probeModelName || p.Prompt != "ping" {
					t.Errorf("image payload = %+v", p)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name+"/400判健康(回归gpustack_tts误判)", func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				if r.Method != http.MethodPost {
					t.Errorf("method = %q, want POST", r.Method)
				}
				body, _ := io.ReadAll(r.Body)
				if c.kind != "asr" && c.verifyBody != nil {
					c.verifyBody(t, body)
				} else if c.kind == "asr" {
					// multipart：验证能解析出占位模型名
					if !strings.Contains(r.Header.Get("Content-Type"), "multipart/form-data") {
						t.Errorf("asr Content-Type = %q, want multipart/form-data", r.Header.Get("Content-Type"))
					}
					if !strings.Contains(string(body), probeModelName) {
						t.Errorf("asr body missing placeholder model %q", probeModelName)
					}
				}
				// 典型上游拒绝场景：TTS 无 chat template / 模型路由层不认识占位名 → 400
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"model not found","code":400}}`)
			}))
			defer srv.Close()

			up := config.Upstream{Name: "kind-" + c.kind, Type: "openai", Kind: c.kind, BaseURL: srv.URL, APIKey: "sk-test"}
			ck := New(testCfg(up))
			defer ck.Close()

			out := ck.ping(context.Background(), &upstreamState{up: &up, timeout: 2 * time.Second})
			if gotPath != c.wantPath {
				t.Errorf("probe path = %q, want %q (kind=%q)", gotPath, c.wantPath, c.kind)
			}
			if !out.ok {
				t.Errorf("4xx probe ok = false, want healthy (out=%+v)", out)
			}
		})
	}

	// 5xx 仍失败：连续两次 → degraded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	up := config.Upstream{Name: "tts-broken", Kind: "tts", BaseURL: srv.URL, APIKey: "k"}
	ck := New(testCfg(up))
	defer ck.Close()
	st := ck.states[up.Name]
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ck.checkOnce(ctx, st)
	ck.checkOnce(ctx, st)
	if got := ck.Status(up.Name); got != StateDegraded {
		t.Errorf("5xx status = %q, want degraded", got)
	}
}

// mustDecode 解码 JSON 测试请求体。
func mustDecode(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
}
