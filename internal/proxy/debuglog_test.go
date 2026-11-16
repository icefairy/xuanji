package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/debuglog"
	"github.com/icefairy/xuanji/internal/router"
	"github.com/tidwall/gjson"
)

// ===== 测试辅助 =====

// debugTestEnv 描述一个带调试日志的多上游测试环境。
type debugTestEnv struct {
	h      *Handler
	dir    string
	writer *debuglog.Writer
	// hits 记录每个上游被请求的次数（按下标）。
	// 用 atomic：httptest server 每个请求在独立 goroutine，普通 int 自增会被 -race 报数据竞争。
	hits []atomic.Int32
	// maxRetries 是 post 时写入的跨上游重试预算（默认 0：只试一次）。
	// 单候选失败的用例若允许重试，同一上游会被反复重试，日志里出现多条同名记录，
	// 掩盖「每个异常候选各一条」的语义。
	maxRetries int
}

// newDebugTestEnv 构造 env；upstreams 每个元素是该上游的处理函数（nil 表示返回 200 正常响应）。
// enabled 控制调试开关；启用时返回非 nil writer（测试结束时 Close）。
func newDebugTestEnv(t *testing.T, enabled bool, upstreams ...http.HandlerFunc) *debugTestEnv {
	return newDebugTestEnvOrder(t, enabled, nil, upstreams...)
}

// newDebugTestEnvOrder 同 newDebugTestEnv，但可指定 routing 规则里的**上游尝试顺序**。
//
// 为什么需要：selectCandidates 在「全部上游都不健康」时会兜底按 tier 选一个，
// 而正常情况按 weight 降序（同权重随机打乱）。测试要断言「先试谁、后试谁」时，
// 必须显式控制顺序，否则用例会随调度策略变化而假红/假绿。
func newDebugTestEnvOrder(t *testing.T, enabled bool, order []int, upstreams ...http.HandlerFunc) *debugTestEnv {
	t.Helper()
	cfg := &config.Config{
		// 默认不重试：本组用例验证的是「一次尝试的记录内容与归因」，
		// 重试会计数多条记录（那是 TestDebugLog_RetryAttemptsAreNumbered 的事）。
		Retry: config.Retry{MaxRetries: 0, RetryStatuses: []int{429, 500, 502, 503, 504}},
	}
	cfg.Proxy.DebugRequestLog = enabled

	env := &debugTestEnv{hits: make([]atomic.Int32, len(upstreams)), maxRetries: 0}
	var names []string
	for i, fn := range upstreams {
		name := fmt.Sprintf("up%d", i+1)
		names = append(names, name)
		var srv *httptest.Server
		idx := i
		if fn == nil {
			fn = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
			}
		}
		inner := fn
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			env.hits[idx].Add(1)
			inner(w, r)
		}))
		t.Cleanup(srv.Close)
		cfg.Upstreams = append(cfg.Upstreams, config.Upstream{
			Name: name, BaseURL: srv.URL + "/v1", APIKey: "sk-test",
			// 同 tier 内 selectCandidates 按**有效权重**降序排；权重相同时会随机打乱。
			// 因此这里让 weight 严格递减，测试才能稳定断言「第一个候选是谁」。
			// （Priority 字段不参与同 tier 排序，不能用它来定序。）
			Priority: 10, Weight: 100 - i*10, Models: []string{"m"},
		})
	}
	ordered := names
	if len(order) == len(names) && len(order) > 0 {
		ordered = make([]string, 0, len(order))
		for _, idx := range order {
			ordered = append(ordered, names[idx])
		}
	}
	cfg.Routing = config.Routing{
		DefaultStrategy: "primary_backup",
		Rules:           []config.Rule{{Model: "m", Upstreams: ordered, Strategy: "primary_backup"}},
	}
	rt := router.New(cfg)
	// 不能传 health checker：新建的 Checker 起点是「未知」，HealthyUpstreams 会判定
	// 全部不健康并走 tier 兜底（只返回 1 个候选），多候选场景就测不出来。
	// 生产环境探活完成后才有健康数据；这里模拟「全部健康」。
	env.h = New(cfg, rt, nil)
	if enabled {
		env.dir = filepath.Join(t.TempDir(), "debug-logs")
		w, err := debuglog.New(env.dir, debuglog.Options{})
		if err != nil {
			t.Fatalf("debuglog.New: %v", err)
		}
		// 不注册 t.Cleanup(Close)：records() 要按 writer.Path() 读文件，
		// 而 Path() 在 Close 之后仍可读，但保持句柄存活让测试结束时一次性回收更简单。
		t.Cleanup(func() { w.Close() })
		env.writer = w
		env.h.SetDebugLog(w)
		env.writer = w
	}
	return env
}

// records 读取当前日志并解析为每条记录。
func (e *debugTestEnv) records(t *testing.T) []map[string]any {
	t.Helper()
	if e.writer == nil {
		return nil
	}
	// 关闭前 flush（Close 幂等，测试结束后 cleanup 会再次调用）
	if err := e.writer.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	path := e.writer.Path()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read %s: %v", path, err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("日志行不是合法 JSON: %v\n%s", err, line)
		}
		out = append(out, m)
	}
	return out
}

// post 发一次 chat 请求并确保调试日志已落盘。
//
// 为什么每次都 Sync：Handler 的调试日志 defer 与 httptest.Recorder 的响应写入是
// 两件独立的事，某些路径（forwardOnce 的 defer）在 ChatCompletions 返回后仍可能
// 处于 flush 中；测试里显式 Sync 才能稳定读到全部记录。
func (e *debugTestEnv) post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	// 单候选失败的用例只希望有一个「尝试」：重试预算默认设 0。
	// 需要验证多候选归属/切换的用例显式提升（见 setRetries）。
	e.h.cfg.Retry.MaxRetries = e.maxRetries
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// 带一个必须**不落盘**的凭证头，用于验证日志不含 secret
	req.Header.Set("Authorization", "Bearer sk-SECRET-CLIENT-KEY")
	req.Header.Set("Cookie", "session=SECRET-COOKIE")
	rec := httptest.NewRecorder()
	e.h.ChatCompletions(rec, req)
	if e.writer != nil {
		if err := e.writer.Sync(); err != nil {
			t.Fatalf("sync: %v", err)
		}
	}
	return rec
}

func mustString(t *testing.T, v any) string {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("字段不是字符串: %#v", v)
	}
	return s
}

// ===== 用例 =====

// TestDebugLog_DisabledWritesNothing 开关关闭时：不创建文件、不记录任何内容。
func TestDebugLog_DisabledWritesNothing(t *testing.T) {
	env := newDebugTestEnv(t, false, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"message":"boom"}}`)
	})
	env.post(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if env.writer != nil {
		t.Fatal("关闭时不应有 writer")
	}
	// 关闭时 Handler 的 debugLog 指针应为 nil：整条链路走零开销分支
	if env.h.debugReady() != nil {
		t.Fatal("关闭时 debugReady 应返回 nil")
	}
}

// TestDebugLog_DisabledNoDir 开关关闭 + writer 从未注入时，目录不应被创建。
func TestDebugLog_DisabledNoDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "never")
	env := newDebugTestEnv(t, false, nil)
	env.post(t, `{"model":"m","messages":[]}`)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("关闭时不应创建目录: %v", err)
	}
}

// TestDebugLog_Upstream5xxRecordsUpstreamAndModel 单候选 500：精确归因到该上游与真实模型。
func TestDebugLog_Upstream5xxRecordsUpstreamAndModel(t *testing.T) {
	env := newDebugTestEnv(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"message":"upstream exploded"}}`)
	})
	rec := env.post(t, `{"model":"m","messages":[{"role":"user","content":"你好\n\"引号\""}]}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("上游 500 不应返回 200: %s", rec.Body.String())
	}
	rows := env.records(t)
	if len(rows) != 1 {
		t.Fatalf("记录数 = %d, want 1: %#v", len(rows), rows)
	}
	r := rows[0]
	if got := mustString(t, r["upstream"]); got != "up1" {
		t.Errorf("upstream = %q, want up1", got)
	}
	if got := mustString(t, r["client_model"]); got != "m" {
		t.Errorf("client_model = %q, want m", got)
	}
	if got := r["upstream_status"].(float64); got != 500 {
		t.Errorf("upstream_status = %v, want 500", got)
	}
	if got := r["final_status"].(float64); got == 0 {
		t.Errorf("final_status 不应为 0（最终失败必须给出下游终态）")
	}
	if got := r["final"].(bool); !got {
		t.Errorf("唯一候选失败应标记 final=true")
	}
	if got := r["attempt"].(float64); got != 1 {
		t.Errorf("attempt = %v, want 1", got)
	}
	if got := r["endpoint"]; got != "chat" {
		t.Errorf("endpoint = %v, want chat", got)
	}
	if reqID := mustString(t, r["request_id"]); reqID == "" {
		t.Errorf("request_id 不应为空")
	}
	errStr := mustString(t, r["error"])
	if !strings.Contains(errStr, "500") {
		t.Errorf("error 应含上游状态: %q", errStr)
	}
}

// TestDebugLog_MultiCandidateAttribution 多候选：中间失败 final=false、最终失败 final=true，
// 各自归属自己的 upstream / upstream_model / upstream_status。
func TestDebugLog_MultiCandidateAttribution(t *testing.T) {
	env := newDebugTestEnvOrder(t, true, []int{0, 1},
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, `{"error":{"message":"first down"}}`)
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"rate limited"}}`)
		},
	)
	env.maxRetries = 2 // 两个候选都要被尝试到
	env.h.cfg.Upstreams[0].ModelMapping = map[string]string{"m": "real-model-1"}
	env.h.cfg.Upstreams[1].ModelMapping = map[string]string{"m": "real-model-2|real-model-3"}

	env.post(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	rows := env.records(t)
	if len(rows) != 2 {
		t.Fatalf("两个候选各失败一次应落 2 条, got %d: %#v", len(rows), rows)
	}
	// 记录按「本次请求的尝试顺序」写入；候选顺序是权重优先（非配置顺序），
	// 这里用 upstream 名反查期望值，避免依赖打乱结果。
	first, second := rows[0], rows[1]
	if first["attempt"].(float64) != 1 || second["attempt"].(float64) != 2 {
		t.Fatalf("attempt 顺序错乱: %v / %v", first["attempt"], second["attempt"])
	}
	// 两个候选各归自己的状态码：up1=502、up2=429（与尝试顺序无关）
	wantStatus := map[string]float64{"up1": 502, "up2": 429}
	seen := map[string]bool{}
	for _, r := range rows {
		name := mustString(t, r["upstream"])
		seen[name] = true
		if got := r["upstream_status"].(float64); got != wantStatus[name] {
			t.Errorf("%s upstream_status = %v, want %v", name, got, wantStatus[name])
		}
	}
	if !seen["up1"] || !seen["up2"] {
		t.Errorf("两个候选未各落一条: %v", seen)
	}
	if first["final"].(bool) {
		t.Errorf("中间候选失败应为 final=false（之后切到了下一个候选）")
	}
	if !second["final"].(bool) {
		t.Errorf("最终候选失败应为 final=true")
	}
	if first["final_status"].(float64) != 0 {
		t.Errorf("中间候选 final_status 应为 0（下游终态尚未确定）")
	}
	if got := second["final_status"].(float64); got != http.StatusBadGateway {
		t.Errorf("最终候选 final_status = %v, want 502", got)
	}
	// 多模型映射：upstream_model 必须是真实模型名（不是聚合名 m）
	if got := mustString(t, first["upstream_model"]); got == "" || got == "m" {
		t.Errorf("upstream_model = %q, 应为上游真实模型名", got)
	}
	// 同一请求的所有候选必须共用 request_id
	if mustString(t, first["request_id"]) != mustString(t, second["request_id"]) {
		t.Errorf("同一次请求的候选 request_id 不一致")
	}
}

// TestDebugLog_FailThenSuccessKeepsFailedCandidate 先失败后成功：
// 上游 500 → 上游 200。必须保留失败候选的异常记录，且该记录 final=false。
func TestDebugLog_FailThenSuccessKeepsFailedCandidate(t *testing.T) {
	env := newDebugTestEnvOrder(t, true, []int{0, 1},
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":{"message":"boom"}}`)
		},
		nil, // up2 正常
	)
	env.maxRetries = 2 // 需要尝试第二个候选
	rec := env.post(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("应切换成功, got %d: %s", rec.Code, rec.Body.String())
	}
	rows := env.records(t)
	if len(rows) != 1 {
		t.Fatalf("成功切换后仍应保留失败候选记录, got %d", len(rows))
	}
	r := rows[0]
	if mustString(t, r["upstream"]) != "up1" {
		t.Errorf("upstream = %v, want up1", r["upstream"])
	}
	if r["final"].(bool) {
		t.Errorf("失败候选之后切换成功 → final 必须为 false")
	}
	if r["final_status"].(float64) != 0 {
		t.Errorf("最终请求成功 → final_status 应为 0, got %v", r["final_status"])
	}
}

// TestDebugLog_RetryAttemptsAreNumbered 同一上游被重试多次：每次尝试都要各落一条，
// attempt 递增，且**只有最后一条** final=true —— 早期每次都自称 final 是错的。
func TestDebugLog_RetryAttemptsAreNumbered(t *testing.T) {
	var hits int32
	env := newDebugTestEnv(t, true, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"message":"always boom"}}`)
	})
	env.maxRetries = 2 // 现有语义：单候选时会重试一次，共 2 次尝试
	env.post(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("上游被调用 %d 次, want 2（首试 + 1 次重试）", got)
	}
	rows := env.records(t)
	if len(rows) != 2 {
		t.Fatalf("记录数 = %d, want 2（每次尝试各一条）: %#v", len(rows), rows)
	}
	for i, r := range rows {
		if got := r["attempt"].(float64); int(got) != i+1 {
			t.Errorf("第 %d 条 attempt = %v, want %d（重试必须递增）", i, got, i+1)
		}
		if got := mustString(t, r["upstream"]); got != "up1" {
			t.Errorf("第 %d 条 upstream = %q, want up1", i, got)
		}
		wantFinal := i == len(rows)-1
		if got := r["final"].(bool); got != wantFinal {
			t.Errorf("第 %d 条 final = %v, want %v（只有最后一次尝试是终态）", i, got, wantFinal)
		}
		if !wantFinal && r["final_status"].(float64) != 0 {
			t.Errorf("第 %d 条非终态 final_status = %v, want 0", i, r["final_status"])
		}
	}
	if got := rows[len(rows)-1]["final_status"].(float64); got == 0 {
		t.Error("最后一次尝试必须给出 final_status")
	}
}

// TestDebugLog_ConnectionErrorRecordsZeroStatus 连接失败（上游不可达）：upstream_status 记 0。
func TestDebugLog_ConnectionErrorRecordsZeroStatus(t *testing.T) {
	env := newDebugTestEnv(t, true, nil)
	// 把唯一上游指向一个必然拒连的地址
	env.h.cfg.Upstreams[0].BaseURL = "http://127.0.0.1:1/v1"
	env.post(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	rows := env.records(t)
	if len(rows) != 1 {
		t.Fatalf("记录数 = %d, want 1", len(rows))
	}
	if got := rows[0]["upstream_status"].(float64); got != 0 {
		t.Errorf("连接失败 upstream_status = %v, want 0", got)
	}
	if !rows[0]["final"].(bool) {
		t.Error("唯一候选连接失败应标记 final=true")
	}
	errStr := mustString(t, rows[0]["error"])
	if !strings.Contains(errStr, "upstream request failed") {
		t.Errorf("error 应包含连接失败原因: %q", errStr)
	}
}

// TestDebugLog_PassThrough4xxRecordsFinal 可重试的 4xx（如 413 不可重试透传）：
// 上游真实状态与下游终态都保留。
func TestDebugLog_NonRetryable4xxPassThrough(t *testing.T) {
	env := newDebugTestEnv(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		fmt.Fprint(w, `{"error":{"message":"payload too large"}}`)
	})
	// MaxRetries 默认 2，但 413 不在 retry_statuses 且不含关键词 → 不可重试
	rec := env.post(t, `{"model":"m","messages":[{"role":"user","content":"`+strings.Repeat("a", 64)+`"}]}`)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("应透传 413, got %d: %s", rec.Code, rec.Body.String())
	}
	rows := env.records(t)
	if len(rows) != 1 {
		t.Fatalf("记录数 = %d, want 1", len(rows))
	}
	r := rows[0]
	if got := r["upstream_status"].(float64); got != 413 {
		t.Errorf("upstream_status = %v, want 413", got)
	}
	if got := r["final_status"].(float64); got != 413 {
		t.Errorf("final_status = %v, want 413", got)
	}
	if !r["final"].(bool) {
		t.Error("不可重试的透传 4xx 即最终结果, final 应为 true")
	}
}

// TestDebugLog_RawRequestBodyVerbatim 原始 body 原样 round-trip：中文/引号/换行/超长不截断，
// 且 raw_request 保留客户端模型名（未被网关改写），upstream_request 记录改写后的模型名。
func TestDebugLog_RawRequestBodyVerbatim(t *testing.T) {
	env := newDebugTestEnv(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"message":"boom"}}`)
	})
	env.h.cfg.Upstreams[0].ModelMapping = map[string]string{"m": "real/upstream-model"}

	long := strings.Repeat("长文本 段落\"引号\"\n第二行\t制表", 2000)
	body := `{"model":"m","temperature":0.7,"messages":[{"role":"user","content":` +
		mustJSONStr(t, long) + `}]}`
	env.post(t, body)

	rows := env.records(t)
	if len(rows) != 1 {
		t.Fatalf("记录数 = %d, want 1", len(rows))
	}
	raw, err := json.Marshal(rows[0]["raw_request"])
	if err != nil {
		t.Fatal(err)
	}
	var gotRaw map[string]any
	if err := json.Unmarshal(raw, &gotRaw); err != nil {
		t.Fatalf("raw_request 不是合法 JSON 对象: %v", err)
	}
	if got := gotRaw["model"]; got != "m" {
		t.Errorf("raw_request.model = %v, want 客户端原始值 m", got)
	}
	msgs := gotRaw["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].(string)
	if content != long {
		t.Errorf("raw_request 内容未原样 round-trip（len got=%d want=%d）", len(content), len(long))
	}

	upReq, ok := rows[0]["upstream_request"].(map[string]any)
	if !ok {
		t.Fatalf("upstream_request 缺失或不是对象: %#v", rows[0]["upstream_request"])
	}
	if got := upReq["model"]; got != "real/upstream-model" {
		t.Errorf("upstream_request.model = %v, want 真实上游模型名", got)
	}
}

// TestDebugLog_InvalidJSONBodySafe 非法 JSON 请求体不得破坏 NDJSON 行结构。
func TestDebugLog_InvalidJSONBodySafe(t *testing.T) {
	env := newDebugTestEnv(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"message":"boom"}}`)
	})
	// 非法 JSON（缺引号/截断）但含 model 字面量便于路由
	env.post(t, `{"model":"m", "messages":[{"role":"user","content":"unterminated}`)

	rows := env.records(t) // records() 内部会校验每行都是合法 JSON
	if len(rows) != 1 {
		t.Fatalf("记录数 = %d, want 1", len(rows))
	}
	raw, _ := rows[0]["raw_request"].(string)
	if !strings.Contains(raw, "unterminated") {
		t.Errorf("非法 body 应以字符串形式原样保留, got %q", raw)
	}
}

// TestDebugLog_NoSecretHeaders 日志中不得出现 Authorization / Cookie 等凭证。
func TestDebugLog_NoSecretHeaders(t *testing.T) {
	env := newDebugTestEnv(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"message":"boom"}}`)
	})
	env.post(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	data, err := os.ReadFile(env.writer.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"SECRET-CLIENT-KEY", "SECRET-COOKIE", "sk-test"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("日志泄露凭证 %q:\n%s", secret, string(data))
		}
	}
	for _, key := range []string{"Authorization", "Cookie", "X-Api-Key"} {
		if strings.Contains(string(data), key) {
			t.Errorf("日志出现请求头字段名 %q", key)
		}
	}
}

// TestDebugLog_ConcurrentRequests 并发请求各自落自己的记录，互不串行错乱。
func TestDebugLog_ConcurrentRequests(t *testing.T) {
	env := newDebugTestEnv(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"message":"boom"}}`)
	})
	const n = 24
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":"req-%d"}]}`, i)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			env.h.ChatCompletions(httptest.NewRecorder(), req)
		}(i)
	}
	wg.Wait()
	rows := env.records(t)
	if len(rows) != n {
		t.Fatalf("并发记录数 = %d, want %d", len(rows), n)
	}
	ids := make(map[string]bool, n)
	for _, r := range rows {
		id := mustString(t, r["request_id"])
		if ids[id] {
			t.Fatalf("并发请求 request_id 重复: %s", id)
		}
		ids[id] = true
		content := gjson.GetBytes(mustJSONBytes(t, r["raw_request"]), "messages.0.content").String()
		if content == "" {
			t.Errorf("并发记录缺少 raw_request 内容: %#v", r)
		}
	}
}

// TestDebugLog_HotSwapAndClose 热替换：写入新 writer 后旧 writer 不再收到记录；
// 关闭（SetDebugLog(nil)）后不再写任何文件。
func TestDebugLog_HotSwapAndClose(t *testing.T) {
	env := newDebugTestEnv(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"message":"boom"}}`)
	})
	w1 := env.writer
	env.post(t, `{"model":"m","messages":[]}`)
	first := w1.Path()
	if got := len(env.records(t)); got != 1 {
		t.Fatalf("第一次记录数 = %d, want 1", got)
	}

	// 热替换成新目录的 writer
	dir2 := t.TempDir()
	w2, err := debuglog.New(dir2, debuglog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	env.h.SetDebugLog(w2)
	env.writer = w2 // records() 之后读新 writer

	env.post(t, `{"model":"m","messages":[]}`)
	data2, err := os.ReadFile(w2.Path())
	if err != nil || len(data2) == 0 {
		t.Fatalf("新 writer 未收到记录: %v", err)
	}
	before, _ := os.ReadFile(first)
	env.post(t, `{"model":"m","messages":[]}`)
	if err := w2.Sync(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(first)
	if len(before) != len(after) {
		t.Errorf("热替换后旧 writer 仍在写入（%d → %d）", len(before), len(after))
	}

	// 关闭：不再写
	env.h.SetDebugLog(nil)
	if env.h.debugReady() != nil {
		t.Fatal("SetDebugLog(nil) 后 debugReady 应返回 nil")
	}
	beforeClose, _ := os.ReadFile(w2.Path())
	env.post(t, `{"model":"m","messages":[]}`)
	afterClose, _ := os.ReadFile(w2.Path())
	if len(afterClose) != len(beforeClose) {
		t.Error("关闭后仍在写入")
	}
}

// ===== 小工具 =====

func mustJSONStr(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustJSONBytes(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// errBoom 供需要 error 的桩使用。
var errBoom = errors.New("boom")
