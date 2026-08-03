// Package health 实现上游健康检查与状态维护。
//
// 每个上游独立定时探测 GET {base_url}/models（带 Authorization），
// 状态机为 healthy → degraded（连续 2 次失败）→ dead（连续 5 次失败或检查超时），
// dead 后以一半间隔继续探测，成功一次即回 healthy。
package health

import (
	"context"
	"errors"
	"io"
	"log/slog"
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
	DefaultInterval = 30 * time.Second
	// DefaultTimeout 是单次健康检查超时的默认值。
	DefaultTimeout = 5 * time.Second
	// degradedAfterFails 是进入 degraded 所需连续失败次数。
	degradedAfterFails = 2
	// deadAfterFails 是进入 dead 所需连续失败次数。
	deadAfterFails = 5
	// recoveryProbeDivisor 是 dead 后恢复探测间隔相对正常间隔的分母。
	recoveryProbeDivisor = 2
)

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
func (c *Checker) checkOnce(ctx context.Context, st *upstreamState) {
	ok, timedOut, latency := c.ping(ctx, st)
	if c.recorder != nil {
		c.recorder.RecordProbe(st.up.Name, ok, time.Now())
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case ok:
		st.fails = 0
		st.current = StateHealthy
		st.latency = latency
		st.ProbeSuccess++
	case timedOut:
		st.fails = deadAfterFails
		st.current = StateDead
		st.latency = 0
		st.ProbeFail++
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
	}
	c.log.Info("health check",
		"upstream", st.up.Name,
		"state", st.current,
		"fails", st.fails,
		"latency", st.latency.String(),
	)
}

// ping 探测上游：GET {base_url}/models（OpenAI）或 /api/tags（Ollama），2xx 视为健康。
// 返回是否健康、是否因超时失败，以及本次探测的往返延迟。
// ⚠ base_url 不带 /v1 时探测路径必须拼 /v1/models：部分上游（商汤日日新、基元律动）
// 只认 /v1/ 前缀，打 {base_url}/models 会 404 导致误判 dead（与 chatPath 同源坑，2026-08 修复）。
func (c *Checker) ping(ctx context.Context, st *upstreamState) (ok, timedOut bool, latency time.Duration) {
	target := strings.TrimRight(st.up.BaseURL, "/")
	if st.up.IsOllama() {
		target += "/api/tags"
	} else {
		target += "/models"
	}
	reqCtx, cancel := context.WithTimeout(ctx, st.timeout)
	defer cancel()

	start := time.Now()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
	if err != nil {
		return false, false, 0
	}
	req.Header.Set("Authorization", "Bearer "+st.up.APIKey)

	resp, err := c.client.Do(req)
	latency = time.Since(start)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
			return false, true, 0
		}
		return false, false, 0
	}
	defer resp.Body.Close()
	// 排空响应体以复用连接
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode >= 200 && resp.StatusCode < 300, false, latency
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
