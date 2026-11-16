package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/router"
	"github.com/icefairy/xuanji/internal/store"
)

// 自适应权重（按真实请求成功率降权 / 按历史故障段时长预测复权）的常量。
//
// 背景（2026-11 实测）：workbuddy/deepseek-v4.1-flash 窗口成功率 24.9%（账号池耗尽，
// 连续 538 分钟无一成功），却因静态 weight 排在 codebuddy（98.3%）前面，白吞流量。
// 因此降权要快（窗口成功率），复权不能统一固定速率——瞬时 429（中位 6 秒恢复）与
// 额度耗尽（数小时）必须区别对待，见 probeAt 的计算。
const (
	// adaptiveMinFactor 是降权下限：再低也要保留极小权重而不是 0，
	// 否则「有效权重 = int(Weight*factor)」可能低于 1 的下限，候选被排除。
	adaptiveMinFactor = 0.05
	// adaptiveProbeFactor 是到 probe_at 之后的试探系数：放少量流量验证是否恢复。
	adaptiveProbeFactor = 0.3
	// adaptiveDefaultRecovery 是无历史故障段时的默认恢复时长（10 分钟）。
	adaptiveDefaultRecovery = 10 * time.Minute
	// adaptiveProbeSafety 是试探时机的乘数（1.5 倍中位恢复时长）。
	adaptiveProbeSafety = 1.5
	// adaptiveQuotaProbeDelay 是「额度耗尽」类失败的固定长退避（6 小时）。
	adaptiveQuotaProbeDelay = 6 * time.Hour
	// adaptiveMaxBackoffDelay 是退避上限（24 小时）。
	adaptiveMaxBackoffDelay = 24 * time.Hour
	// adaptiveProbeMinSamples 是判定试探成功所需的最小样本数。
	adaptiveProbeMinSamples = 3
	// adaptiveHistoryWindow 是故障段历史分析的回看时长。
	adaptiveHistoryWindow = 7 * 24 * time.Hour
	// adaptiveHistoryRowLimit 是单 (上游,模型) 故障段分析的明细行上限。
	adaptiveHistoryRowLimit = 20000
	// adaptiveStatTimeout 是单次统计查询的超时：request_log 可能有百万行，
	// 查询慢不能拖死后台 goroutine（也不影响转发热路径）。
	adaptiveStatTimeout = 20 * time.Second
)

// adaptiveKey 构造缓存 key。用 \x00 分隔避免「上游名含 :: 」这类歧义。
func adaptiveKey(upstream, model string) string {
	return upstream + "\x00" + model
}

// adaptiveEntry 是一个 (上游, 模型) 的自适应状态。
type adaptiveEntry struct {
	upstream    string
	model       string
	factor      float64 // 当前惩罚系数（1.0 = 无惩罚）
	rate        float64 // 最近一次判定时的成功率快照
	samples     int     // 最近一次判定的样本数
	windowHours int     // 判定时的统计窗口
	// probeAt 语义二选一（由 probeStart 是否为零值区分）：
	//   - 非试探期：下次可试探时间；
	//   - 试探期：本次观察期结束（可以判定结果）的时刻。
	probeAt time.Time
	// probeStart 是本次试探的起点：试探期判定只统计 ts >= probeStart 的流量
	// （否则历史 429 会把成功的试探压回降权，见 TASK-probe-fix.md）。进试探时写入，退出时清零。
	probeStart  time.Time
	backoff     int           // 连续失败次数（指数退避用）
	probeWindow time.Duration // 试探观察期（进试探时按故障段预测写入）
}

// inProbe 判断条目是否处于试探观察期（probeStart 非零 = 试探中）。
func (e adaptiveEntry) inProbe() bool { return !e.probeStart.IsZero() }

// AdaptiveCache 维护「有效权重 = upstreams.weight * factor」的惩罚系数。
//
// 铁律（见 TASK-adaptive-priority.md）：
//   - 只动有效权重，**永不改写** upstreams.weight / priority；
//   - EffectiveWeight 是转发热路径：只读内存 + RLock，无 SQL、无阻塞；
//   - 不触发 reloadConfig（重建 handler 代价过高），改完靠内存缓存生效。
//
// cfg 字段随构建时快照，热重载时由 main 重建注入（统计状态在 DB，不丢）。
type AdaptiveCache struct {
	mu      sync.RWMutex
	entries map[string]*adaptiveEntry // key = upstream + "\x00" + model

	store *store.Store
	// src 是可注入的统计查询源（缺失时用 store；测试注入假数据）。
	src    Samples
	router *router.Router
	log    *slog.Logger

	enabled       bool
	windowHours   int
	threshold     float64
	recover       float64
	minSamples    int
	refresh       time.Duration
	routeCountFor func(model string) int            // 路由规则里该模型的候选上游数（单上游不调整）
	baseWeight    func(upstream string) (int, bool) // 按上游名取配置 weight（日志展示用）

	now func() time.Time // 时钟注入点（测试用）
}

// NewAdaptiveCache 依据配置创建缓存（store/router 允许为 nil，此时功能降级为不调权）。
func NewAdaptiveCache(cfg *config.Config, st *store.Store, rt *router.Router) *AdaptiveCache {
	a := &AdaptiveCache{
		entries:     make(map[string]*adaptiveEntry),
		store:       st,
		router:      rt,
		log:         slog.Default(),
		now:         time.Now,
		windowHours: config.DefaultAdaptiveWindowHours,
		threshold:   config.DefaultAdaptiveThreshold,
		recover:     config.DefaultAdaptiveRecover,
		minSamples:  config.DefaultAdaptiveMinSamples,
		refresh:     time.Duration(config.DefaultAdaptiveRefreshSeconds) * time.Second,
	}
	a.applyConfig(cfg)
	return a
}

// Update 热更新配置与路由引用（热重载复用同一实例时调用）：只刷新快照，
// 不动内存里的惩罚状态（状态以 DB 为准，下一轮 tick 继续生效）。
func (a *AdaptiveCache) Update(cfg *config.Config, rt *router.Router) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.router = rt
	a.mu.Unlock()
	a.applyConfig(cfg)
}

// applyConfig 把配置快照进缓存（nil 配置时按「关闭」处理，保持零行为变化）。
func (a *AdaptiveCache) applyConfig(cfg *config.Config) {
	if cfg == nil {
		a.enabled = false
		return
	}
	p := cfg.Proxy
	a.enabled = p.AdaptivePriority
	if p.AdaptiveWindowHours > 0 {
		a.windowHours = p.AdaptiveWindowHours
	} // 0 = 当天，由 windowStart 处理
	if p.AdaptiveThreshold > 0 && p.AdaptiveThreshold <= 1 {
		a.threshold = p.AdaptiveThreshold
	}
	if p.AdaptiveRecover > 0 && p.AdaptiveRecover <= 1 {
		a.recover = p.AdaptiveRecover
	}
	if a.recover < a.threshold { // 迟滞带不允许反转
		a.recover = a.threshold
	}
	if p.AdaptiveMinSamples > 0 {
		a.minSamples = p.AdaptiveMinSamples
	}
	if p.AdaptiveRefreshSeconds > 0 {
		a.refresh = time.Duration(p.AdaptiveRefreshSeconds) * time.Second
	}
}

// SetBaseWeightLookup 注入「按上游名取配置 weight」的回调（日志展示用，nil 安全）。
func (a *AdaptiveCache) SetBaseWeightLookup(fn func(upstream string) (int, bool)) {
	if a == nil {
		return
	}
	a.baseWeight = fn
}

// Enabled 返回开关状态（关闭时 EffectiveWeight 原样返回 Weight）。
func (a *AdaptiveCache) Enabled() bool { return a != nil && a.enabled }

// EffectiveWeight 返回上游对某模型的有效权重（转发热路径，必须无 SQL、无阻塞）。
//
// 严格按 (上游, 客户端模型名) 取系数：不同模型配额的独立上游
// （deepseek-v4.1-flash 耗尽而 deepseek-v4-flash 正常）正是本功能的主要场景，
// 因此不做「上游级」回退混淆。下限 1：有效权重不能变成 0 或被排除。
func (a *AdaptiveCache) EffectiveWeight(u *config.Upstream, model string) int {
	if a == nil || u == nil {
		if u == nil {
			return 0
		}
		return u.Weight
	}
	if !a.enabled || a.entries == nil {
		return u.Weight
	}
	a.mu.RLock()
	e := a.entries[adaptiveKey(u.Name, model)]
	var factor float64
	if e != nil {
		factor = e.factor
	}
	a.mu.RUnlock()
	if e == nil || factor >= 1 {
		return u.Weight
	}
	w := int(float64(u.Weight) * factor)
	if w < 1 {
		w = 1
	}
	return w
}

// factorFor 返回当前系数（1.0 表示无惩罚）。
func (a *AdaptiveCache) factorFor(upstream, model string) float64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if e := a.entries[adaptiveKey(upstream, model)]; e != nil {
		return e.factor
	}
	return 1
}

// Load 从 DB 恢复已持久化的系数（启动时调用，保证重启后降权不归零）。
func (a *AdaptiveCache) Load() {
	if a == nil || a.store == nil {
		return
	}
	rows, err := a.store.ListAdaptiveWeights()
	if err != nil {
		a.log.Warn("adaptive: load weights failed", "error", err)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range rows {
		f := r.Factor
		if f <= 0 || f > 1 {
			f = 1
		}
		a.entries[adaptiveKey(r.Upstream, r.Model)] = &adaptiveEntry{
			upstream:    r.Upstream,
			model:       r.Model,
			factor:      f,
			rate:        r.Rate,
			samples:     r.Samples,
			windowHours: r.WindowHours,
			probeAt:     parseAdaptiveTime(r.ProbeAt),
			probeStart:  parseAdaptiveTime(r.ProbeStart),
			backoff:     r.Backoff,
		}
	}
	if len(rows) > 0 {
		a.log.Info("adaptive: loaded persisted weights", "rows", len(rows))
	}
}

// Start 加载持久化状态并启动后台刷新 goroutine。返回 stop 函数（幂等）。
// 开关关闭时直接返回 no-op：零 goroutine、零 SQL、零行为变化。
func (a *AdaptiveCache) Start(ctx context.Context) func() {
	if a == nil || !a.enabled {
		return func() {}
	}
	a.Load()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		// 启动后立即刷新一次，不等第一个 tick（重启后尽快恢复降权状态）
		a.refreshOnce()
		t := time.NewTicker(a.refresh)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				a.refreshOnce()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}

// refreshOnce 执行一轮统计与调权。panic 不能杀掉后台 goroutine。
func (a *AdaptiveCache) refreshOnce() {
	defer func() {
		if r := recover(); r != nil {
			a.log.Error("adaptive: refresh panic", "panic", r)
		}
	}()
	if a.samples() == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), adaptiveStatTimeout)
	defer cancel()
	a.tick(ctx, a.now())
}

// Samples 是统计查询结果的抽象（生产用 store，测试可注入假数据）。
type Samples interface {
	AdaptiveSamples(since string) ([]store.AdaptiveSample, error)
	// AdaptiveSamplesSince 额外限定 after（RFC3339，空串=不限定），
	// 供试探期只统计「试探起点之后」的新流量。
	AdaptiveSamplesSince(since, after string) ([]store.AdaptiveSample, error)
	AdaptiveProbeRows(upstream, model, since string, limit int) ([]store.AdaptiveProbeRow, error)
}

// samples 返回统计查询源（未注入时为 store；两者都缺失时返回 nil，由调用方跳过）。
func (a *AdaptiveCache) samples() Samples {
	if a.src != nil {
		return a.src
	}
	if a.store == nil {
		return nil
	}
	return a.store
}

// tick 是 refreshOnce 的可测试内核：先降权/试探/恢复（纯内存），再读库。
func (a *AdaptiveCache) tick(ctx context.Context, now time.Time) {
	// 阶段一：已有效果的条目——到 probe_at 进试探、试探期满按结果恢复或退避。
	a.evaluateEntries(now)
	if a.samples() == nil {
		return
	}
	// 阶段二：用最新窗口成功率调整「无惩罚」条目 / 继续降权已惩罚条目。
	// 注意：这里必须用完整窗口（降权判定要看全量历史）；试探期判定另走自己的口径（见 applySamples）。
	since := a.windowSince(now)
	samples, err := a.samples().AdaptiveSamples(since)
	if err != nil {
		if ctx.Err() == nil {
			a.log.Warn("adaptive: sample query failed", "error", err)
		}
		return
	}
	a.applySamples(now, samples)
}

// probeSamples 取「试探观察期内」的样本：ts >= max(窗口起点, 试探起点)，
// 且只保留当前处于试探期的条目。失败时返回 nil（本轮试探判定跳过，不降级成全窗口）。
func (a *AdaptiveCache) probeSamples(now time.Time, inProbe map[string]bool) []store.AdaptiveSample {
	if len(inProbe) == 0 {
		return nil
	}
	var earliest time.Time
	for k := range inProbe {
		a.mu.RLock()
		e := a.entries[k]
		a.mu.RUnlock()
		if e == nil || e.probeStart.IsZero() {
			continue
		}
		if earliest.IsZero() || e.probeStart.Before(earliest) {
			earliest = e.probeStart
		}
	}
	if earliest.IsZero() {
		return nil
	}
	after := earliest
	if winStart := parseAdaptiveTime(a.windowSince(now)); !winStart.IsZero() && winStart.After(after) {
		after = winStart // 取 max(窗口起点, 试探起点)
	}
	all, err := a.samples().AdaptiveSamplesSince(a.windowSince(now), after.UTC().Format(time.RFC3339))
	if err != nil {
		a.log.Warn("adaptive: probe sample query failed", "error", err)
		return nil
	}
	out := make([]store.AdaptiveSample, 0, len(inProbe))
	for _, s := range all {
		if inProbe[adaptiveKey(s.Upstream, s.Model)] {
			out = append(out, s)
		}
	}
	return out
}

// adaptiveWindowTZ 是「当天」窗口的业务时区。项目其他统计口径统一用东八区
// （见 store 里 `strftime('%Y-%m-%d', ts, '+8 hours')`），窗口起点必须与之对齐，
// 否则「当天」会差 8 小时（用 now.Location() 时，now 为 UTC 的进程会算成 UTC 零点）。
var adaptiveWindowTZ = time.FixedZone("CST", 8*3600)

// windowSince 返回统计窗口起点（UTC RFC3339，与 request_log.ts 同格式）。
// windowHours == 0 表示「东八区当天 00:00」。
func (a *AdaptiveCache) windowSince(now time.Time) string {
	if a.windowHours <= 0 {
		t := now.In(adaptiveWindowTZ)
		y, m, d := t.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, adaptiveWindowTZ).UTC().Format(time.RFC3339)
	}
	return now.Add(-time.Duration(a.windowHours) * time.Hour).UTC().Format(time.RFC3339)
}

// evaluateEntries 处理已惩罚条目的试探/恢复/退避（不读库，结果写回 DB）。
func (a *AdaptiveCache) evaluateEntries(now time.Time) {
	a.mu.RLock()
	list := make([]adaptiveEntry, 0, len(a.entries))
	for _, e := range a.entries {
		list = append(list, *e)
	}
	a.mu.RUnlock()
	// 按 upstream/model 排序，保证日志与判定顺序稳定（可复现）
	sort.Slice(list, func(i, j int) bool {
		if list[i].upstream != list[j].upstream {
			return list[i].upstream < list[j].upstream
		}
		return list[i].model < list[j].model
	})
	for _, e := range list {
		if e.probeAt.After(now) {
			continue // 还没到试探时间，保持惩罚
		}
		if e.factor > 0 && e.factor < adaptiveProbeFactor {
			// 到点：临时提升到试探系数，放出少量流量
			a.probeUp(e, now)
		}
	}
}

// probeUp 把条目临时提升到试探系数，并设定试探观察期（用于判定试探结果）。
func (a *AdaptiveCache) probeUp(e adaptiveEntry, now time.Time) {
	window := a.predictRecovery(e.upstream, e.model, now)
	if window < adaptiveDefaultRecovery {
		window = adaptiveDefaultRecovery
	}
	e.factor = adaptiveProbeFactor
	e.probeWindow = window
	// probeStart 标记试探起点：观察期内的判定只统计这个时刻之后的流量。
	e.probeStart = now
	// probeAt 在试探期语义下表示「观察期结束、可以判定结果」的时刻。
	// 观察期内若已提前恢复到恢复阈，applySamples 会直接完全恢复。
	e.probeAt = now.Add(window)
	a.setEntry(e, now)
	a.log.Info("adaptive: probe",
		"target", e.upstream+"/"+logModel(e.model),
		"factor", adaptiveProbeFactor, "backoff", e.backoff,
		"next", formatAdaptiveTime(e.probeAt))
}

// applySamples 依据窗口成功率调整系数（降权 / 试探结果判定）。
func (a *AdaptiveCache) applySamples(now time.Time, samples []store.AdaptiveSample) {
	// 按模型聚合上游，用于「单上游模型不调整」判定
	byModel := make(map[string]int, 16)
	for _, s := range samples {
		byModel[s.Model]++
	}
	// 试探中的条目：全窗口样本只用于降权/恢复判定，试探结果另取「试探起点之后」的样本。
	// 否则历史 291 次 429 会把试探期 10 次成功的成功率算成 3.4%，被降权上游永远无法恢复。
	inProbe := make(map[string]bool, 8)
	a.mu.RLock()
	for _, e := range a.entries {
		if e.inProbe() {
			inProbe[adaptiveKey(e.upstream, e.model)] = true
		}
	}
	a.mu.RUnlock()
	probeSamples := a.probeSamples(now, inProbe)
	probeByKey := make(map[string]store.AdaptiveSample, len(probeSamples))
	for _, s := range probeSamples {
		probeByKey[adaptiveKey(s.Upstream, s.Model)] = s
	}
	for _, s := range samples {
		rate := 0.0
		if s.Total > 0 {
			rate = float64(s.OK) / float64(s.Total)
		}
		key := adaptiveKey(s.Upstream, s.Model)
		// 试探期用「新流量」口径：以试探起点之后的样本为准；
		// 该条目在试探起点后完全没有新流量时，Total=0（而不是历史全窗口）——
		// 这正是「样本不足保持试探」所依赖的信号。
		probeRate, probeTotal := rate, s.Total
		if inProbe[key] {
			ps := probeByKey[key]
			probeTotal = ps.Total
			probeRate = 0
			if ps.Total > 0 {
				probeRate = float64(ps.OK) / float64(ps.Total)
			}
		}
		a.mu.RLock()
		e := a.entries[key]
		var cur adaptiveEntry
		if e != nil {
			cur = *e
		}
		a.mu.RUnlock()

		// 单上游模型不调整：路由里该模型只有 1 个上游时降权只会更糟。
		if a.routeCountForModel(s.Model, byModel) < 2 {
			if e != nil {
				a.recoverEntry(cur, now)
			}
			continue
		}

		switch {
		case e == nil:
			// 无惩罚条目：样本够、成功率低于降权阈才降权
			if s.Total < a.minSamples || rate >= a.threshold {
				continue
			}
			factor := clampFactor(rate / a.threshold)
			entry := adaptiveEntry{
				upstream:    s.Upstream,
				model:       s.Model,
				factor:      factor,
				rate:        rate,
				samples:     s.Total,
				windowHours: a.windowHours,
				backoff:     0,
			}
			a.probeDelay(&entry, now)
			a.setEntry(entry, now)
			a.logAdjust(entry, nil)

		case cur.factor <= adaptiveProbeFactor:
			// 试探期判定一律用「试探起点之后」的样本（probeRate/probeTotal）。
			if now.Before(cur.probeAt) {
				// 观察期内：新流量成功率已回到恢复阈 → 提前完全恢复。
				if probeTotal >= adaptiveProbeMinSamples && probeRate >= a.recover {
					a.recoverEntry(cur, now)
					continue
				}
				// 否则保持试探系数继续观察（样本不足/仍失败都不推进退避）
				cur.rate = probeRate
				cur.samples = probeTotal
				a.setEntry(cur, now)
				continue
			}
			// 观察期结束：样本够且恢复到恢复阈 → 完全恢复；样本不足 → 继续试探；否则退避。
			if probeTotal < adaptiveProbeMinSamples {
				cur.rate = probeRate
				cur.samples = probeTotal
				cur.probeAt = now.Add(cur.probeWindow) // 顺延观察期，继续保持试探系数
				a.setEntry(cur, now)
				a.log.Info("adaptive: probe extend",
					"target", cur.upstream+"/"+logModel(cur.model),
					"factor", round3(cur.factor), "n", probeTotal,
					"next", formatAdaptiveTime(cur.probeAt))
				continue
			}
			if probeRate >= a.recover {
				a.recoverEntry(cur, now)
				continue
			}
			next := a.backoffEntry(cur, probeRate, probeTotal, now)
			a.setEntry(next, now)
			a.logProbeFailed(next)

		default:
			// 已有惩罚（> 试探系数）：继续降权（成功率更低时）或提前恢复（成功率回到恢复阈）。
			if s.Total >= a.minSamples && rate >= a.recover {
				a.recoverEntry(cur, now)
				continue
			}
			if s.Total < a.minSamples || rate >= a.threshold {
				continue
			}
			next := cur
			next.factor = clampFactor(rate / a.threshold)
			next.rate = rate
			next.samples = s.Total
			next.windowHours = a.windowHours
			a.probeDelay(&next, now)
			a.setEntry(next, now)
			a.logAdjust(next, &cur)
		}
	}
}

// routeCountForModel 返回路由里该模型的候选上游数（同模型的所有上游，跨 tier）。
// 优先用 samples 里的出现次数；若该模型在窗口内只有 1 个上游有流量（另一个上游
// 完全没被调用到，正是「被降权到没人用」的典型形态），回退查路由规则。
func (a *AdaptiveCache) routeCountForModel(model string, seen map[string]int) int {
	n := seen[model]
	if n >= 2 {
		return n
	}
	if a.router != nil {
		if ups, _, err := a.router.Route(model); err == nil && len(ups) > n {
			return len(ups)
		}
	}
	return n
}

// probeDelay 依据历史 429 故障段时长预测下次试探时间（需求书第五节）。
//
//   - 有「账号池无可用账号 / no accounts」类错误 → 额度类，固定 6 小时；
//   - 否则用中位恢复时长 * 1.5；无历史（<2 段）时用默认 10 分钟。
//
// 段还在持续、从未恢复的情况，用「当前时间 - 段起点」作为该段时长下限。
func (a *AdaptiveCache) probeDelay(e *adaptiveEntry, now time.Time) {
	rec := a.predictRecovery(e.upstream, e.model, now)
	// 额度耗尽类（账号池无可用账号）固定 6 小时，不再乘 1.5：
	// 需求书给的是绝对时长，乘系数会变成 9 小时，明显偏离预期。
	if rec == adaptiveQuotaProbeDelay {
		e.probeAt = now.Add(adaptiveQuotaProbeDelay)
		return
	}
	delay := time.Duration(float64(rec) * adaptiveProbeSafety)
	if delay <= 0 {
		delay = time.Duration(float64(adaptiveDefaultRecovery) * adaptiveProbeSafety)
	}
	e.probeAt = now.Add(delay)
}

// predictRecovery 返回该 (上游,模型) 的中位恢复时长；额度类失败返回 6 小时。
func (a *AdaptiveCache) predictRecovery(upstream, model string, now time.Time) time.Duration {
	if a.samples() == nil {
		return adaptiveDefaultRecovery
	}
	since := now.Add(-adaptiveHistoryWindow).UTC().Format(time.RFC3339)
	rows, err := a.samples().AdaptiveProbeRows(upstream, model, since, adaptiveHistoryRowLimit)
	if err != nil {
		a.log.Warn("adaptive: probe history query failed", "upstream", upstream, "model", logModel(model), "error", err)
		return adaptiveDefaultRecovery
	}
	stats := analyzeFaultSegments(rows, now)
	if stats.quotaExhaust {
		return adaptiveQuotaProbeDelay
	}
	if stats.segments < 2 {
		return adaptiveDefaultRecovery
	}
	return stats.median
}

// faultStats 是故障段分析结果。
type faultStats struct {
	segments     int
	median       time.Duration
	quotaExhaust bool // 历史里出现过额度耗尽类错误（账号池无可用账号等）
}

// analyzeFaultSegments 从明细行里识别「连续 429、中间无 2xx」的故障段并取时长中位数。
// 段时长 = 最后一次 429 时间 - 第一次 429 时间；仍在持续（末尾无 2xx）的段用
// 「当前时间 - 段起点」作为时长下限（需求书 5.1）。
func analyzeFaultSegments(rows []store.AdaptiveProbeRow, now time.Time) faultStats {
	var st faultStats
	var segStart, segEnd int64 // 当前故障段的起止（ms），segStart=0 表示不在故障段
	var durations []time.Duration
	flush := func(endMS int64) {
		if segStart == 0 {
			return
		}
		d := time.Duration(endMS-segStart) * time.Millisecond
		if d < 0 {
			d = 0
		}
		durations = append(durations, d)
		segStart, segEnd = 0, 0
	}
	for _, r := range rows {
		if r.AtMS == 0 {
			continue
		}
		switch {
		case r.Status == 429:
			if segStart == 0 {
				segStart = r.AtMS
			}
			segEnd = r.AtMS
			if isQuotaExhaustText(r.ErrText) {
				st.quotaExhaust = true
			}
		case r.Status >= 200 && r.Status <= 299:
			// 2xx 结束当前故障段
			flush(segEnd)
		}
	}
	// 末尾仍在故障段：用当前时间作为下限
	if segStart != 0 {
		flush(now.UnixNano() / int64(time.Millisecond))
	}
	st.segments = len(durations)
	st.median = medianDuration(durations)
	return st
}

// medianDuration 返回中位数（偶数取中间两值均值）。
func medianDuration(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	cp := make([]time.Duration, len(ds))
	copy(cp, ds)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	mid := len(cp) / 2
	if len(cp)%2 == 1 {
		return cp[mid]
	}
	return (cp[mid-1] + cp[mid]) / 2
}

// quotaExhaustKeywords 是「额度耗尽」类错误特征：恢复时间长（6 小时量级），
// 与瞬时并发限流（中位数秒级）区别对待。小写匹配。
var quotaExhaustKeywords = []string{
	"账号池无可用账号",
	"无可用账号",
	"no accounts",
	"no available account",
	"额度已用完",
	"额度耗尽",
}

// isQuotaExhaustText 判断错误详情是否属于额度耗尽。
func isQuotaExhaustText(s string) bool {
	if s == "" {
		return false
	}
	lower := strings.ToLower(s)
	for _, kw := range quotaExhaustKeywords {
		if strings.Contains(lower, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

// backoffEntry 计算试探失败后的退避状态。
// probe_at = now + medianRecovery * 1.5 * 2^backoff（上限 24 小时）；factor 回到低值。
func (a *AdaptiveCache) backoffEntry(cur adaptiveEntry, rate float64, samples int, now time.Time) adaptiveEntry {
	next := cur
	next.backoff = cur.backoff + 1
	next.rate = rate
	next.samples = samples
	next.windowHours = a.windowHours
	next.probeStart = time.Time{} // 退出试探期：下次判定回到全窗口口径
	// 因子回到「按最近成功率算出的低值」，至少保持惩罚
	f := clampFactor(rate / a.threshold)
	if f > adaptiveProbeFactor {
		f = adaptiveProbeFactor
	}
	next.factor = f
	base := a.predictRecovery(next.upstream, next.model, now)
	if base <= 0 {
		base = adaptiveDefaultRecovery
	}
	if base == adaptiveQuotaProbeDelay {
		// 额度类：已经是最长量级的退避，直接 6 小时（再乘/再翻倍没有意义）
		next.factor = f
		next.probeAt = now.Add(adaptiveQuotaProbeDelay)
		next.probeWindow = 0
		return next
	}
	// 指数退避：medianRecovery * 1.5 * 2^backoff（上限 24 小时）。
	// 注意减掉已过去的试探观察期，否则等待时间会比预期多一个观察期。
	delay := time.Duration(float64(base) * adaptiveProbeSafety * math.Pow(2, float64(next.backoff-1)))
	if delay > adaptiveMaxBackoffDelay {
		delay = adaptiveMaxBackoffDelay
	}
	if w := cur.probeWindow; w > 0 && w < delay {
		delay -= w
	}
	next.probeAt = now.Add(delay)
	next.probeWindow = 0
	return next
}

// recoverEntry 完全恢复：删除内存行 + DB 行、backoff 清零。
func (a *AdaptiveCache) recoverEntry(e adaptiveEntry, now time.Time) {
	if e.factor >= 1 {
		return
	}
	key := adaptiveKey(e.upstream, e.model)
	a.mu.Lock()
	delete(a.entries, key)
	a.mu.Unlock()
	if a.store != nil {
		if err := a.store.DeleteAdaptiveWeight(e.upstream, e.model); err != nil {
			a.log.Warn("adaptive: delete weight failed", "upstream", e.upstream,
				"model", logModel(e.model), "error", err)
		}
	}
	a.log.Info("adaptive: recover", "upstream", e.upstream, "model", logModel(e.model),
		"factor", 1.0, "rate", percent(e.rate), "n", e.samples, "at", formatAdaptiveTime(now))
}

// setFactor 只改系数（试探提升用），保留 probeAt/backoff 等状态。
func (a *AdaptiveCache) setFactor(e adaptiveEntry, factor float64, now time.Time) {
	e.factor = factor
	a.setEntry(e, now)
}

// setEntry 写内存 + 落库（DB 失败只告警，不影响内存生效）。
func (a *AdaptiveCache) setEntry(e adaptiveEntry, now time.Time) {
	a.mu.Lock()
	if a.entries == nil {
		a.entries = make(map[string]*adaptiveEntry)
	}
	cp := e
	a.entries[adaptiveKey(e.upstream, e.model)] = &cp
	a.mu.Unlock()
	if a.store == nil {
		return
	}
	rec := store.AdaptiveWeight{
		Upstream:    e.upstream,
		Model:       e.model,
		Factor:      e.factor,
		Rate:        e.rate,
		Samples:     e.samples,
		WindowHours: e.windowHours,
		ProbeAt:     formatAdaptiveTime(e.probeAt),
		ProbeStart:  formatAdaptiveTime(e.probeStart),
		Backoff:     e.backoff,
		UpdatedAt:   formatAdaptiveTime(now),
	}
	if err := a.store.UpsertAdaptiveWeight(rec); err != nil {
		a.log.Warn("adaptive: upsert weight failed", "upstream", e.upstream,
			"model", logModel(e.model), "error", err)
	}
}

// logAdjust 打降权日志（仅在系数/样本发生变化时调用方才会走到这里）。
// 示例：adaptive: adjust workbuddy/deepseek-v4.1-flash weight 211→87 (factor=0.415 rate=24.9% n=374)
func (a *AdaptiveCache) logAdjust(e adaptiveEntry, prev *adaptiveEntry) {
	if prev != nil && math.Abs(prev.factor-e.factor) < 1e-6 && prev.samples == e.samples {
		return // 无变化不打日志（60s 一次会刷屏）
	}
	w := adaptiveBaseWeight(a, e.upstream)
	oldW := effWeight(w, 1)
	if prev != nil {
		oldW = effWeight(w, prev.factor)
	}
	a.log.Info("adaptive: adjust",
		"target", e.upstream+"/"+logModel(e.model),
		"weight", fmt.Sprintf("%d→%d", oldW, effWeight(w, e.factor)),
		"factor", round3(e.factor), "rate", percent(e.rate), "n", e.samples)
}

// logProbeFailed 打试探失败日志。
func (a *AdaptiveCache) logProbeFailed(e adaptiveEntry) {
	a.log.Info("adaptive: probe failed",
		"target", e.upstream+"/"+logModel(e.model),
		"factor", round3(e.factor), "rate", percent(e.rate), "n", e.samples,
		"backoff", e.backoff, "next", formatAdaptiveTime(e.probeAt))
}

// adaptiveBaseWeightLookup 是 adaptiveBaseWeight 的可注入版本，由 Handler 注入
// 「按上游名取配置 weight」的回调（Handler 持有完整 cfg，能精确取值）。
// 未注入时返回 100 的展示兜底：日志只用于解释顺序变化，量级正确即可。
func adaptiveBaseWeight(a *AdaptiveCache, upstream string) int {
	if a != nil && a.baseWeight != nil {
		if w, ok := a.baseWeight(upstream); ok {
			return w
		}
	}
	return 100
}

// effWeight 计算有效权重（与 EffectiveWeight 同口径），供日志使用。
func effWeight(weight int, factor float64) int {
	if factor >= 1 {
		return weight
	}
	w := int(float64(weight) * factor)
	if w < 1 {
		w = 1
	}
	return w
}

// clampFactor 把 rate/threshold 夹到 [adaptiveMinFactor, 1.0]。
func clampFactor(f float64) float64 {
	if f < adaptiveMinFactor {
		return adaptiveMinFactor
	}
	if f > 1 {
		return 1
	}
	return f
}

func round3(f float64) float64 { return math.Round(f*1000) / 1000 }

// percent 把 0~1 的比率格式化为带一位小数的百分数（日志用）。
func percent(r float64) string { return strconv.FormatFloat(math.Round(r*1000)/10, 'f', 1, 64) + "%" }

// logModel 把空模型名（上游级）表示为 "-"，避免日志出现空的 model=。
func logModel(m string) string {
	if m == "" {
		return "-"
	}
	return m
}

// formatAdaptiveTime 统一时间格式（UTC RFC3339，秒精度）。
func formatAdaptiveTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// parseAdaptiveTime 解析 DB 里的时间（兼容 RFC3339 与 'YYYY-MM-DD HH:MM:SS'）。
func parseAdaptiveTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.UTC); err == nil {
		return t
	}
	return time.Time{}
}

// Snapshot 是管理端展示用的一行快照。
type AdaptiveSnapshot struct {
	Upstream    string  `json:"upstream"`
	Model       string  `json:"model"`
	Factor      float64 `json:"factor"`
	Rate        float64 `json:"rate"`
	Samples     int     `json:"samples"`
	WindowHours int     `json:"window_hours"`
	ProbeAt     string  `json:"probe_at"`
	ProbeStart  string  `json:"probe_start"`
	Backoff     int     `json:"backoff"`
	UpdatedAt   string  `json:"updated_at"`
}

// Snapshot 返回当前内存中的全部惩罚状态（管理端展示用；不查库）。
func (a *AdaptiveCache) Snapshot() []AdaptiveSnapshot {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]AdaptiveSnapshot, 0, len(a.entries))
	for _, e := range a.entries {
		out = append(out, AdaptiveSnapshot{
			Upstream:    e.upstream,
			Model:       e.model,
			Factor:      e.factor,
			Rate:        e.rate,
			Samples:     e.samples,
			WindowHours: e.windowHours,
			ProbeAt:     formatAdaptiveTime(e.probeAt),
			ProbeStart:  formatAdaptiveTime(e.probeStart),
			Backoff:     e.backoff,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Upstream != out[j].Upstream {
			return out[i].Upstream < out[j].Upstream
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// ForceRefreshForTest 立即执行一轮统计刷新（运维排查/外部校验用；生产由 ticker 驱动）。
// 不写 DB 之外的任何状态，安全可重复调用。
func (a *AdaptiveCache) ForceRefreshForTest(ctx context.Context) {
	if a == nil || !a.enabled {
		return
	}
	a.Load()
	a.tick(ctx, a.now())
}

// SetClockForTest 注入时钟（仅测试/离线校验用；生产使用 time.Now）。
func (a *AdaptiveCache) SetClockForTest(now func() time.Time) {
	if a == nil || now == nil {
		return
	}
	a.now = now
}
