// Package health 实现上游健康检查与状态维护。
//
// 每个上游独立定时探测，状态机为 healthy → degraded（连续 2 次失败）→ dead（连续 5 次失败
// 或检查超时），dead 后以一半间隔继续探测，成功一次即回 healthy。
//
// 无凭证探测（2026-08-24 统一）：所有探测请求均不携带 Authorization，零 token 消耗、
// 免真实推理等待。探测端点按上游能力（Upstream.Kind）选择：
//
//	chat（默认）→ POST /chat/completions（404/405 回退 POST /embeddings）
//	emb → POST /embeddings
//	rerank → POST /rerank
//	tts → POST /audio/speech
//	asr → POST /audio/transcriptions
//	image → POST /images/generations
//
// 判定语义（2026-08-26 统一为「无凭证探活」）：2xx 与全部 4xx（400/401/403/404/405/429）
// 均视为健康——探测请求不带凭证，有鉴权上游在鉴权层 401/403 早拒；无鉴权上游因占位模型名
// （probeModelName，不存在的模型）在模型路由/校验层即被 400 拒绝。两者都不进入真实推理
// （零算力消耗），且请求已穿透到应用层被业务逻辑处理，恰好证明网络通、端点存在、服务进程活。
// 典型收益：纯 TTS 上游被 chat 端点探测时返回 400 "no chat template"，此前误判 dead，
// 现在正确判为健康。仅 5xx / 超时 / 连接失败判定失败。key 有效性与推理链路由 proxy 层
// fastfail 探测（带真实 key 的最小 chat 请求）负责，分层互补。
package health

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/icefairy/xuanji/internal/config"
)

// State 是上游健康状态的枚举值。
type State string

const (
	// StateHealthy 表示上游正常。
	StateHealthy State = "healthy"
	// StateDegraded 表示上游连续失败、已降级但仍在转发候选内。
	StateDegraded State = "degraded"
	// StateDead 表示上游不可用，转发时被排除。
	StateDead State = "dead"
	// StateUnknown 表示未监控的上游。
	StateUnknown State = "unknown"
)

// 默认健康检查参数。
const (
	// DefaultInterval 是健康检查间隔的默认值。
	DefaultInterval = 120 * time.Second
	// DefaultTimeout 是单次健康检查超时的默认值。
	// 2026-08-25 5s→15s：基元律动/tokenrhythm 等上游无凭证探测响应较慢（限流排队/慢生成），
	// 5s 易超时误判 dead；15s 在 30s 探测间隔内仍留有充足冗余且不会拖慢整体节奏。
	DefaultTimeout = 15 * time.Second
	// degradedAfterFails 是进入 degraded 所需连续失败次数。
	degradedAfterFails = 2
	// deadAfterFails 是进入 dead 所需连续失败次数。
	deadAfterFails = 5
	// recoveryProbeDivisor 是 dead 后恢复探测间隔相对正常间隔的分母。
	recoveryProbeDivisor = 2
)

// maxRespBodyLog 是健康检查失败日志中响应体摘要的最大字符数，防止刷屏。
const maxRespBodyLog = 500

// probeModelName 是所有探测请求统一使用的占位模型名（一个刻意不存在的模型）。
// 用它替代真实模型名做探测，保证探测请求不会触发真实推理：
//   - 有鉴权上游：鉴权层 401/403 早拒，请求到不了模型层；
//   - 无鉴权上游：模型路由/校验层发现模型不存在，返回 400 拒绝，推理引擎不执行；
//
// 两种情况都不消耗算力，且都是健康信号（见 doJSONProbe 的 4xx 豁免语义）。
const probeModelName = "xuanji-probe"

// probeOutcome 描述一次健康检查探测的结果，用于状态更新与排障日志。
type probeOutcome struct {
	ok       bool          // 是否健康（2xx 或 4xx 豁免，见 doJSONProbe）
	timedOut bool          // 是否超时（超时直接判 dead）
	latency  time.Duration // 往返延迟
	status   int           // 非 2xx 时的 HTTP 状态码；4xx 豁免成功时也保留原码供调用方判断（如 chat 404/405 回退）；其余情况为 0
	reason   string        // 失败原因：timeout / 连接错误信息 / non-2xx response
	respBody string        // 非 2xx 时的响应体摘要（截断 maxRespBodyLog 字符）
}

// upstreamState 记录单个上游的健康状态，current/fails/latency 受 Checker.mu 保护。
type upstreamState struct {
	up       *config.Upstream
	interval time.Duration
	timeout  time.Duration

	current State
	fails   int
	latency time.Duration // 最近一次健康检查的往返延迟；失败时为 0

	// 探测统计：健康度 = ProbeSuccess / (ProbeSuccess + ProbeFail)。
	// 定时检测的成功/失败都计入，反映渠道的真实可用率（不只依赖请求结果）。
	ProbeSuccess int64
	ProbeFail    int64
}

// ProbeRecorder 接收每次探测结果，用于持久化统计（metrics 按时间聚合）。
// 由外部（main）注入；nil 时不记录（统计仅依赖内存计数）。
type ProbeRecorder interface {
	RecordProbe(upstream string, ok bool, at time.Time)
}

// Checker 管理所有上游的健康状态。并发安全。
type Checker struct {
	log    *slog.Logger
	client *http.Client
	mu     sync.RWMutex
	states map[string]*upstreamState

	recorder ProbeRecorder // 可选：持久化探测结果

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// SetProbeRecorder 注入探测结果记录器（应在 Start 之前调用）。
// 简单赋值，不持锁：Start 前调用，探测协程运行期间不修改。
func (c *Checker) SetProbeRecorder(r ProbeRecorder) {
	c.recorder = r
}

// New 基于配置构建健康检查器，初始状态一律视为 healthy（乐观），不启动探测。
// 调用方需调用 Start 启动定时检查，并用 Close 释放资源。
func New(cfg *config.Config) *Checker {
	c := &Checker{
		log:    slog.Default(),
		client: &http.Client{},
		states: make(map[string]*upstreamState, len(cfg.Upstreams)),
	}
	for i := range cfg.Upstreams {
		up := &cfg.Upstreams[i]
		interval, timeout := DefaultInterval, DefaultTimeout
		if up.HealthCheck != nil {
			if up.HealthCheck.Interval > 0 {
				interval = time.Duration(up.HealthCheck.Interval)
			}
			if up.HealthCheck.Timeout > 0 {
				timeout = time.Duration(up.HealthCheck.Timeout)
			}
		}
		c.states[up.Name] = &upstreamState{
			up:       up,
			interval: interval,
			timeout:  timeout,
			current:  StateHealthy,
		}
	}
	return c
}

// Start 启动对每个上游的定时健康检查。dead 的上游以 interval/2 的间隔探测。
func (c *Checker) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	for _, st := range c.states {
		c.wg.Add(1)
		go c.loop(ctx, st)
	}
}

// Close 停止所有健康检查并等待 goroutine 退出。可安全重复调用。
func (c *Checker) Close() {
	if c.cancel == nil {
		return
	}
	c.cancel()
	c.wg.Wait()
	c.cancel = nil
}

// loop 是单个上游的检查循环：先立即探测一次，之后按状态切换间隔。
func (c *Checker) loop(ctx context.Context, st *upstreamState) {
	defer c.wg.Done()
	c.checkOnce(ctx, st)
	for {
		interval := st.interval
		if c.isDead(st) {
			interval = st.interval / recoveryProbeDivisor
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			c.checkOnce(ctx, st)
		}
	}
}

// checkOnce 对单个上游执行一次探测并更新状态与延迟。
// 探测的成功/失败同时累加 ProbeSuccess/ProbeFail 统计（健康度指标数据源），
// 并通过 recorder 持久化（若注入）供 metrics 按时间范围聚合。
// 日志约定：成功（healthy）记 Info；失败且状态恶化为 degraded/dead 记 Warn 完整原因；
// 失败但状态保持 healthy（连续失败不足 degradedAfterFails）时不打扰、不记失败日志。
func (c *Checker) checkOnce(ctx context.Context, st *upstreamState) {
	// 禁用（enabled=false）的上游不参与转发路由，健康检查同样停止：
	// 探测结果无人消费，还会对已下线端点持续打请求。状态置 unknown
	// （不参与 healthy/degraded/dead 计数），重新启用并热重载后恢复探测。
	if st.up == nil || !st.up.Enabled {
		c.mu.Lock()
		st.current = StateUnknown
		st.fails = 0
		c.mu.Unlock()
		return
	}
	// 欠费上游停止健康检查：余额不足不会自愈，探测只会继续报错消耗额度窗口；
	// 状态置 unknown（不参与 healthy/degraded/dead 计数），等待人工处理。
	if st.up.Arrears {
		c.mu.Lock()
		st.current = StateUnknown
		st.fails = 0
		c.mu.Unlock()
		return
	}
	out := c.ping(ctx, st)
	if c.recorder != nil {
		c.recorder.RecordProbe(st.up.Name, out.ok, time.Now())
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case out.ok:
		st.fails = 0
		st.current = StateHealthy
		st.latency = out.latency
		st.ProbeSuccess++
		c.log.Info("health check",
			"upstream", st.up.Name,
			"state", st.current,
			"fails", st.fails,
			"latency", st.latency.String(),
		)
	case out.timedOut:
		// 超时直接判 dead，原因固定为 timeout
		st.fails = deadAfterFails
		st.current = StateDead
		st.latency = 0
		st.ProbeFail++
		c.logProbeFailure(st, out)
	default:
		st.fails++
		st.latency = 0
		st.ProbeFail++
		switch {
		case st.fails >= deadAfterFails:
			st.current = StateDead
		case st.fails >= degradedAfterFails:
			st.current = StateDegraded
		default:
			st.current = StateHealthy
		}
		// 状态保持 healthy 时不打扰：不记失败日志，等下次探测恶化了再 Warn
		if st.current == StateHealthy {
			return
		}
		c.logProbeFailure(st, out)
	}
}

// logProbeFailure 记录健康检查失败详情（状态已恶化为 degraded/dead 时调用）。
// 字段统一 snake_case：upstream/state/fails/status/reason/resp_body；
// status 仅在非 2xx 时有值，resp_body 截断 maxRespBodyLog 字符防刷屏。
func (c *Checker) logProbeFailure(st *upstreamState, out probeOutcome) {
	attrs := []any{
		"upstream", st.up.Name,
		"state", st.current,
		"fails", st.fails,
		"reason", out.reason,
	}
	if out.status != 0 {
		attrs = append(attrs, "status", out.status)
	}
	if out.respBody != "" {
		attrs = append(attrs, "resp_body", out.respBody)
	}
	c.log.Warn("health check failed", attrs...)
}

// ping 探测上游：按上游能力（Upstream.Kind）选择探测端点，Ollama 原生走 GET /api/tags。
// 各端点均使用占位模型名 probeModelName（零推理消耗）；判定统一为
// 2xx/4xx 健康、5xx/超时/断连失败（见 doJSONProbe）。
func (c *Checker) ping(ctx context.Context, st *upstreamState) probeOutcome {
	if st.up.IsOllama() {
		return c.ollamaProbe(ctx, st)
	}
	switch st.up.Kind {
	case "emb":
		return c.embeddingsProbe(ctx, st)
	case "rerank":
		return c.rerankProbe(ctx, st)
	case "tts":
		return c.ttsProbe(ctx, st)
	case "asr":
		return c.asrProbe(ctx, st)
	case "image":
		return c.imageProbe(ctx, st)
	default: // "chat" 及历史空值（LoadFromDB 已补默认 chat）
		return c.chatProbe(ctx, st)
	}
}

// ollamaProbe 探测 Ollama 原生服务：GET {base_url}/api/tags，无凭证。
// 判定与其他探测一致的无凭证探活语义：2xx 与 4xx 均健康（应用层在线），5xx/超时/断连失败。
func (c *Checker) ollamaProbe(ctx context.Context, st *upstreamState) probeOutcome {
	target := strings.TrimRight(st.up.BaseURL, "/") + "/api/tags"
	reqCtx, cancel := context.WithTimeout(ctx, st.timeout)
	defer cancel()

	start := time.Now()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
	if err != nil {
		return probeOutcome{reason: "build request: " + err.Error()}
	}

	resp, err := c.client.Do(req)
	latency := time.Since(start)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
			return probeOutcome{timedOut: true, latency: latency, reason: "timeout"}
		}
		return probeOutcome{latency: latency, reason: err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return probeOutcome{ok: true, latency: latency}
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return probeOutcome{ok: true, latency: latency, status: resp.StatusCode}
	default:
		return probeOutcome{
			latency:  latency,
			status:   resp.StatusCode,
			reason:   "non-2xx response",
			respBody: truncateStr(string(body), maxRespBodyLog),
		}
	}
}

// embeddingsProbe 探测 embedding 能力上游（kind=emb，及 kind=chat 探测遇 404/405 时的回退）：
// POST {base_url}/embeddings 最小请求 {"model": probeModelName, "input": "ping"}。
// 占位模型名在模型路由/校验层即被 400 拒绝，不会真实计算向量；
// 判定走统一无凭证探活语义（2xx/4xx 健康，见 doJSONProbe）。
func (c *Checker) embeddingsProbe(ctx context.Context, st *upstreamState) probeOutcome {
	payload, err := json.Marshal(embeddingsProbeReq{Model: probeModelName, Input: "ping"})
	if err != nil {
		return probeOutcome{reason: "embeddings probe: build payload: " + err.Error()}
	}
	return c.doJSONProbe(ctx, st, "/embeddings", payload, "application/json")
}

// embeddingsProbeReq 是 POST /embeddings 探测的最小请求体。
type embeddingsProbeReq struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

// rerankProbeReq / ttsProbeReq / imageProbeReq 分别是 rerank / TTS / 图片生成探测的最小请求体。
type rerankProbeReq struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
}

type ttsProbeReq struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type imageProbeReq struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

// chatProbeMsg / chatProbeReq 是 chat 探测（POST /chat/completions）的最小请求体。
// max_tokens 压到 1：探测只验证端点可用性，不关心回答内容，最小化 token 消耗。
type chatProbeMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatProbeReq struct {
	Model     string         `json:"model"`
	Messages  []chatProbeMsg `json:"messages"`
	MaxTokens int            `json:"max_tokens"`
}

// chatProbe 是 kind=chat 上游的探测方式：POST {base_url}/chat/completions 最小对话请求。
// 模型名用占位名 probeModelName（2026-08-26 起，不再取 ModelMapping/Models 真实模型名）：
// 真实模型名会让无鉴权上游每 30s 真实推理一次；占位模型名在模型路由/校验层即被 400 拒绝，
// 推理引擎不执行，零算力消耗。有鉴权上游则在鉴权层 401/403 早拒。
//
// 历史：不再依赖 GET /models——部分端点对未知 GET 一律返回 400 且无法用作回退信号；
// chat/completions 是所有 OpenAI 兼容端点的最小公共面。
//
// 回退：chat 端点不存在（404/405，典型于仅提供 embeddings 的网关）时回退 POST /embeddings
// 验证并取得真实可用端点的延迟；回退失败也保持 4xx 豁免的健康判定，不再误判 dead。
func (c *Checker) chatProbe(ctx context.Context, st *upstreamState) probeOutcome {
	payload, err := json.Marshal(chatProbeReq{
		Model:     probeModelName,
		Messages:  []chatProbeMsg{{Role: "user", Content: "ping"}},
		MaxTokens: 1,
	})
	if err != nil {
		return probeOutcome{reason: "chat probe: build payload: " + err.Error()}
	}
	out := c.doJSONProbe(ctx, st, "/chat/completions", payload, "application/json")
	// chat 端点不可用（404/405）→ 回退验证 embeddings 端点，取得真实可用延迟；
	// 回退失败（如 embeddings 也不提供）仍保持 4xx 豁免的健康判定。
	if out.ok && (out.status == http.StatusMethodNotAllowed || out.status == http.StatusNotFound) {
		if emb := c.embeddingsProbe(ctx, st); emb.ok {
			return emb
		}
	}
	return out
}

// rerankProbe 探测 rerank 能力上游（kind=rerank）：
// POST {base_url}/rerank 最小请求 {"model": probeModelName, "query": "ping", "documents": ["ping"]}。
// 占位模型名在模型校验层即被 400 拒绝，不会真实执行重排。
func (c *Checker) rerankProbe(ctx context.Context, st *upstreamState) probeOutcome {
	payload, err := json.Marshal(rerankProbeReq{Model: probeModelName, Query: "ping", Documents: []string{"ping"}})
	if err != nil {
		return probeOutcome{reason: "rerank probe: build payload: " + err.Error()}
	}
	return c.doJSONProbe(ctx, st, "/rerank", payload, "application/json")
}

// ttsProbe 探测 TTS 能力上游（kind=tts）：
// POST {base_url}/audio/speech 最小请求 {"model": probeModelName, "input": "ping"}。
// 占位模型名在模型校验层即被 400 拒绝，不会真实合成音频。
func (c *Checker) ttsProbe(ctx context.Context, st *upstreamState) probeOutcome {
	payload, err := json.Marshal(ttsProbeReq{Model: probeModelName, Input: "ping"})
	if err != nil {
		return probeOutcome{reason: "tts probe: build payload: " + err.Error()}
	}
	return c.doJSONProbe(ctx, st, "/audio/speech", payload, "application/json")
}

// imageProbe 探测图片生成能力上游（kind=image）：
// POST {base_url}/images/generations 最小请求 {"model": probeModelName, "prompt": "ping"}。
// 占位模型名在模型校验层即被 400 拒绝，不会真实生成图片。
func (c *Checker) imageProbe(ctx context.Context, st *upstreamState) probeOutcome {
	payload, err := json.Marshal(imageProbeReq{Model: probeModelName, Prompt: "ping"})
	if err != nil {
		return probeOutcome{reason: "image probe: build payload: " + err.Error()}
	}
	return c.doJSONProbe(ctx, st, "/images/generations", payload, "application/json")
}

// asrProbe 探测语音转写能力上游（kind=asr）：
// POST {base_url}/audio/transcriptions，multipart/form-data 最小请求（model + 一个占位 file 字段）。
// 占位模型名在模型校验层即被 400 拒绝，不会真实转录音频；部分实现先解析音频再查模型名，
// 对伪音频返回 400 解码失败——同样属于 4xx 豁免（服务在线），不会误判 dead。
func (c *Checker) asrProbe(ctx context.Context, st *upstreamState) probeOutcome {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("model", probeModelName); err != nil {
		return probeOutcome{reason: "asr probe: write field: " + err.Error()}
	}
	fw, err := mw.CreateFormFile("file", "probe.bin")
	if err != nil {
		return probeOutcome{reason: "asr probe: create form file: " + err.Error()}
	}
	if _, err := fw.Write([]byte("xuanji-probe")); err != nil {
		return probeOutcome{reason: "asr probe: write form file: " + err.Error()}
	}
	if err := mw.Close(); err != nil {
		return probeOutcome{reason: "asr probe: close form: " + err.Error()}
	}
	return c.doJSONProbe(ctx, st, "/audio/transcriptions", buf.Bytes(), mw.FormDataContentType())
}

// doJSONProbe 执行一次最小 POST 探测请求并统一判定（无凭证探活核心，2026-08-26 统一）：
//   - 2xx：健康（服务正常响应）；
//   - 4xx（400/401/403/404/405/429 等全部）：健康——探测请求不携带凭证（无凭证探测），
//     有鉴权上游在鉴权层 401/403 早拒；无鉴权上游因占位模型名 probeModelName 在模型
//     路由/校验层被 400 拒绝。两种情况请求都已穿透到应用层被业务逻辑处理，恰好证明
//     「网络通、端点存在、服务进程活着」，且都不进入真实推理（零算力消耗）。
//     4xx 健康时 status 保留原状态码（供 chatProbe 等做 404/405 端点回退判断）。
//   - 5xx / 超时 / 连接失败：失败（服务真实不可用）。
func (c *Checker) doJSONProbe(ctx context.Context, st *upstreamState, path string, payload []byte, contentType string) probeOutcome {
	target := strings.TrimRight(st.up.BaseURL, "/") + path
	reqCtx, cancel := context.WithTimeout(ctx, st.timeout)
	defer cancel()

	start := time.Now()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return probeOutcome{reason: path + " probe: build request: " + err.Error()}
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := c.client.Do(req)
	latency := time.Since(start)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
			return probeOutcome{timedOut: true, latency: latency, reason: "timeout"}
		}
		return probeOutcome{latency: latency, reason: err.Error()}
	}
	defer resp.Body.Close()
	// 读取响应体：2xx/4xx 时消费以复用连接；5xx 时截断摘要供排障日志
	body, _ := io.ReadAll(resp.Body)
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return probeOutcome{ok: true, latency: latency}
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		// 4xx 豁免：应用层在线（见函数注释）；保留 status 供回退判断
		return probeOutcome{ok: true, latency: latency, status: resp.StatusCode}
	default:
		// 5xx 等其他状态：真实不可用，携带响应体摘要供排障
		return probeOutcome{
			latency:  latency,
			status:   resp.StatusCode,
			reason:   "non-2xx response",
			respBody: truncateStr(string(body), maxRespBodyLog),
		}
	}
}

// truncateStr 截断字符串到 max 字节，超出时末尾加省略号标记。
// 用于日志中的响应体/错误信息，防止超长内容刷屏。
func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

// isDead 判断单个上游当前是否处于 dead。
func (c *Checker) isDead(st *upstreamState) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return st.current == StateDead
}

// Status 返回指定上游的当前状态；未监控的上游返回 StateUnknown。
func (c *Checker) Status(name string) State {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if st, ok := c.states[name]; ok {
		return st.current
	}
	return StateUnknown
}

// Latency 返回指定上游最近一次健康检查的往返延迟；未监控或失败时为 0。
func (c *Checker) Latency(name string) time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if st, ok := c.states[name]; ok {
		return st.latency
	}
	return 0
}

// ProbeRate 返回指定上游定时探测的健康度（成功探测 / 总探测，0~1）。
// 反映程序定时检测的真实成功率，不只依赖请求结果。未监控或无数据时返回 0。
func (c *Checker) ProbeRate(name string) float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	st, ok := c.states[name]
	if !ok {
		return 0
	}
	total := st.ProbeSuccess + st.ProbeFail
	if total == 0 {
		return 0
	}
	return float64(st.ProbeSuccess) / float64(total)
}

// ProbeStats 返回指定上游定时探测的成功/失败次数；未监控时返回 0,0。
func (c *Checker) ProbeStats(name string) (success, fail int64) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if st, ok := c.states[name]; ok {
		return st.ProbeSuccess, st.ProbeFail
	}
	return 0, 0
}

// SortByLatency 将上游列表按最近延迟升序排序（延迟低在前）。
// 无延迟数据（0）的上游排在最后，避免误选未测过的上游。
// 返回排序后的新切片，不修改原切片。
func (c *Checker) SortByLatency(ups []*config.Upstream) []*config.Upstream {
	c.mu.RLock()
	lat := make([]time.Duration, len(ups))
	for i, u := range ups {
		if st, ok := c.states[u.Name]; ok {
			lat[i] = st.latency
		}
	}
	c.mu.RUnlock()

	out := make([]*config.Upstream, len(ups))
	copy(out, ups)
	sort.SliceStable(out, func(i, j int) bool {
		li, lj := lat[indexOf(ups, out[i])], lat[indexOf(ups, out[j])]
		// 无延迟数据排最后
		if li == 0 && lj != 0 {
			return false
		}
		if li != 0 && lj == 0 {
			return true
		}
		return li < lj
	})
	return out
}

// indexOf 返回 up 在 ups 中的索引。
func indexOf(ups []*config.Upstream, up *config.Upstream) int {
	for i, u := range ups {
		if u == up {
			return i
		}
	}
	return 0
}

// HealthyUpstreams 过滤出可用的上游（healthy 或 degraded），排除 dead，
// 保持传入顺序。未监控的上游按健康处理，避免因监控缺失误伤流量。
func (c *Checker) HealthyUpstreams(ups []*config.Upstream) []*config.Upstream {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]*config.Upstream, 0, len(ups))
	for _, u := range ups {
		st, ok := c.states[u.Name]
		if !ok || st.current != StateDead {
			out = append(out, u)
		}
	}
	return out
}

// SetLatencyForTest 仅供测试使用：手动设置指定上游的延迟值。
func (c *Checker) SetLatencyForTest(name string, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if st, ok := c.states[name]; ok {
		st.latency = d
	}
}

// MarkFailure 记录一次转发失败，供 proxy 在请求失败时反馈到健康状态。
// 转发失败是真实请求失败（比健康检查探测更可信），一次即至少降级为 degraded，
// 连续失败达到 deadAfterFails 次后进入 dead。
func (c *Checker) MarkFailure(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.states[name]
	if !ok {
		return
	}
	st.fails++
	switch {
	case st.fails >= deadAfterFails:
		st.current = StateDead
	case st.current == StateHealthy:
		st.current = StateDegraded
	}
	c.log.Warn("upstream failed during proxy forward",
		"upstream", name,
		"state", st.current,
		"fails", st.fails,
	)
}
