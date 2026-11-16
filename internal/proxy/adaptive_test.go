package proxy

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/health"
	"github.com/icefairy/xuanji/internal/router"
	"github.com/icefairy/xuanji/internal/store"
)

// fakeSamples 是统计查询源的测试替身（不碰 SQLite，可精确控制样本与故障段）。
type fakeSamples struct {
	samples []store.AdaptiveSample
	// newSamples 是「试探起点之后」的新流量（可选）：after 非空（试探口径）时替换 samples
	// 才叠加到同名 (upstream, model) 的样本上，用来模拟
	// 「历史全窗口很脏、试探期新流量很干净」的现场。
	newSamples []store.AdaptiveSample
	rows       map[string][]store.AdaptiveProbeRow // key = upstream+"\x00"+model
}

func (f *fakeSamples) AdaptiveSamples(since string) ([]store.AdaptiveSample, error) {
	return f.AdaptiveSamplesSince(since, "")
}

// AdaptiveSamplesSince 返回 [since, ...) 的样本。
// after 为空：返回全窗口 samples；
// after 非空（试探口径）：只返回 after 之后的新流量——即 newSamples，
// 全窗口里压在窗口内的历史失败不得计入（这正是被修复的缺陷）。
func (f *fakeSamples) AdaptiveSamplesSince(since, after string) ([]store.AdaptiveSample, error) {
	if strings.TrimSpace(after) == "" {
		return f.samples, nil
	}
	return f.newSamples, nil
}

func (f *fakeSamples) AdaptiveProbeRows(upstream, model, _ string, _ int) ([]store.AdaptiveProbeRow, error) {
	return f.rows[adaptiveKey(upstream, model)], nil
}

// adaptiveTestCfg 构造一个可控的配置：同 tier 同 weight 的两个上游 + 路由规则。
func adaptiveTestCfg(enabled bool, weights ...int) *config.Config {
	ups := make([]config.Upstream, 0, len(weights))
	names := []string{"workbuddy", "codebuddy", "third"}
	for i, w := range weights {
		ups = append(ups, config.Upstream{Name: names[i], Tier: "free", Weight: w, Enabled: true})
	}
	ruleUps := make([]string, 0, len(ups))
	for _, u := range ups {
		ruleUps = append(ruleUps, u.Name)
	}
	return &config.Config{
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules:           []config.Rule{{Model: "deepseek-v4.1-flash", Upstreams: ruleUps}},
		},
		Upstreams: ups,
		Proxy: config.Proxy{
			AdaptivePriority:       enabled,
			AdaptiveWindowHours:    5,
			AdaptiveThreshold:      0.6,
			AdaptiveRecover:        0.7,
			AdaptiveMinSamples:     10,
			AdaptiveRefreshSeconds: 60,
		},
	}
}

// newAdaptiveForTest 创建一个只走内存的数据源缓存（now 固定，便于断言 probe_at）。
func newAdaptiveForTest(t *testing.T, cfg *config.Config, fake *fakeSamples) (*AdaptiveCache, *Handler, time.Time) {
	t.Helper()
	rt := router.New(cfg)
	hc := health.New(cfg)
	h := New(cfg, rt, hc)
	a := NewAdaptiveCache(cfg, nil, rt)
	a.src = fake
	now := time.Date(2026, 11, 4, 22, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	h.SetAdaptive(a)
	// 日志里的 weight 折算需要原始 weight
	a.SetBaseWeightLookup(func(name string) (int, bool) {
		for i := range cfg.Upstreams {
			if cfg.Upstreams[i].Name == name {
				return cfg.Upstreams[i].Weight, true
			}
		}
		return 0, false
	})
	return a, h, now
}

// 1. 开关关闭时 EffectiveWeight 原样返回 Weight
func TestAdaptive_DisabledReturnsRawWeight(t *testing.T) {
	cfg := adaptiveTestCfg(false, 211, 211)
	fake := &fakeSamples{samples: []store.AdaptiveSample{
		{Upstream: "workbuddy", Model: "deepseek-v4.1-flash", Total: 374, OK: 93},
	}}
	a, h, now := newAdaptiveForTest(t, cfg, fake)
	a.tick(context.Background(), now)

	up := &config.Upstream{Name: "workbuddy", Tier: "free", Weight: 211}
	if got := a.EffectiveWeight(up, "deepseek-v4.1-flash"); got != 211 {
		t.Fatalf("EffectiveWeight = %d, want 211 (开关关闭必须原样返回)", got)
	}
	// 即使内存里被塞了惩罚（例如开关中途关闭），也必须原样返回
	a.mu.Lock()
	a.entries[adaptiveKey("workbuddy", "deepseek-v4.1-flash")] = &adaptiveEntry{
		upstream: "workbuddy", model: "deepseek-v4.1-flash", factor: 0.1,
	}
	a.mu.Unlock()
	if got := a.EffectiveWeight(up, "deepseek-v4.1-flash"); got != 211 {
		t.Fatalf("EffectiveWeight = %d, want 211 (关闭时忽略缓存系数)", got)
	}
	// 排序也不受影响
	ups := []*config.Upstream{
		{Name: "workbuddy", Tier: "free", Weight: 211},
		{Name: "codebuddy", Tier: "free", Weight: 211},
	}
	got := h.selectCandidates(ups, "", "deepseek-v4.1-flash")
	if len(got) != 2 {
		t.Fatalf("candidates = %d, want 2", len(got))
	}
}

// 2. 成功率 25%、样本 374、阈值 0.6 → factor≈0.415，有效权重按比例下降
func TestAdaptive_LowSuccessRateDownweights(t *testing.T) {
	cfg := adaptiveTestCfg(true, 211, 211)
	fake := &fakeSamples{samples: []store.AdaptiveSample{
		{Upstream: "workbuddy", Model: "deepseek-v4.1-flash", Total: 374, OK: 93}, // 24.87%
		{Upstream: "codebuddy", Model: "deepseek-v4.1-flash", Total: 301, OK: 296},
	}}
	a, h, now := newAdaptiveForTest(t, cfg, fake)
	a.tick(context.Background(), now)

	wb := &config.Upstream{Name: "workbuddy", Tier: "free", Weight: 211}
	f := a.factorFor("workbuddy", "deepseek-v4.1-flash")
	// rate/threshold = 0.2487/0.6 = 0.4145
	if math.Abs(f-0.415) > 0.002 {
		t.Fatalf("factor = %v, want ≈0.415", f)
	}
	if got, want := a.EffectiveWeight(wb, "deepseek-v4.1-flash"), int(211*f); got != want {
		t.Fatalf("EffectiveWeight = %d, want %d", got, want)
	}
	// 高成功率上游不受影响
	cb := &config.Upstream{Name: "codebuddy", Tier: "free", Weight: 211}
	if got := a.EffectiveWeight(cb, "deepseek-v4.1-flash"); got != 211 {
		t.Fatalf("codebuddy EffectiveWeight = %d, want 211", got)
	}
	// probe_at 被设置（不是零值）
	a.mu.RLock()
	e := a.entries[adaptiveKey("workbuddy", "deepseek-v4.1-flash")]
	a.mu.RUnlock()
	if e == nil || !e.probeAt.After(now) {
		t.Fatalf("probeAt = %v, want future time", e)
	}
	if got := h.effectiveWeight(wb, "deepseek-v4.1-flash"); got == wb.Weight {
		t.Fatalf("handler 未使用自适应权重：effectiveWeight = %d，应小于 %d", got, wb.Weight)
	}
}

// 3. 样本数 < minSamples 时不调整
func TestAdaptive_BelowMinSamplesNoAdjust(t *testing.T) {
	cfg := adaptiveTestCfg(true, 100, 100)
	fake := &fakeSamples{samples: []store.AdaptiveSample{
		{Upstream: "workbuddy", Model: "deepseek-v4.1-flash", Total: 9, OK: 0},
		{Upstream: "codebuddy", Model: "deepseek-v4.1-flash", Total: 50, OK: 50},
	}}
	a, h, now := newAdaptiveForTest(t, cfg, fake)
	a.tick(context.Background(), now)

	if f := a.factorFor("workbuddy", "deepseek-v4.1-flash"); f != 1 {
		t.Fatalf("factor = %v, want 1 (样本不足不调整)", f)
	}
	up := &config.Upstream{Name: "workbuddy", Tier: "free", Weight: 100}
	if got := h.effectiveWeight(up, "deepseek-v4.1-flash"); got != 100 {
		t.Fatalf("effectiveWeight = %d, want 100", got)
	}
}

// 4. 成功率 65%（介于 0.6 与 0.7 之间）→ 迟滞生效，不改变原状态
func TestAdaptive_HysteresisNoChangeInDeadBand(t *testing.T) {
	cfg := adaptiveTestCfg(true, 100, 100)
	// 先制造一次降权（25%）
	fake := &fakeSamples{samples: []store.AdaptiveSample{
		{Upstream: "workbuddy", Model: "deepseek-v4.1-flash", Total: 100, OK: 25},
		{Upstream: "codebuddy", Model: "deepseek-v4.1-flash", Total: 100, OK: 100},
	}}
	a, _, now := newAdaptiveForTest(t, cfg, fake)
	a.tick(context.Background(), now)
	before := a.factorFor("workbuddy", "deepseek-v4.1-flash")
	if before >= 1 {
		t.Fatalf("precondition: factor = %v, want < 1", before)
	}

	// 成功率回到 65%：介于 0.6 与 0.7 之间，保持原因子不变（不抖动）
	fake.samples[0] = store.AdaptiveSample{Upstream: "workbuddy", Model: "deepseek-v4.1-flash", Total: 100, OK: 65}
	now2 := now.Add(10 * time.Minute)
	a.tick(context.Background(), now2)
	if after := a.factorFor("workbuddy", "deepseek-v4.1-flash"); math.Abs(after-before) > 1e-9 {
		t.Fatalf("factor = %v, want %v (迟滞带内不得改变)", after, before)
	}

	// 无惩罚条目在迟滞带内也不应被降权
	fake.samples[1] = store.AdaptiveSample{Upstream: "codebuddy", Model: "deepseek-v4.1-flash", Total: 100, OK: 65}
	a.tick(context.Background(), now2.Add(time.Minute))
	if f := a.factorFor("codebuddy", "deepseek-v4.1-flash"); f != 1 {
		t.Fatalf("codebuddy factor = %v, want 1 (65%% 高于降权阈 60%%)", f)
	}
}

// 5. 单上游模型不调整
func TestAdaptive_SingleUpstreamModelNotAdjusted(t *testing.T) {
	cfg := &config.Config{
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules:           []config.Rule{{Model: "solo-model", Upstreams: []string{"workbuddy"}}},
		},
		Upstreams: []config.Upstream{{Name: "workbuddy", Tier: "free", Weight: 100, Enabled: true}},
		Proxy: config.Proxy{
			AdaptivePriority: true, AdaptiveWindowHours: 5, AdaptiveThreshold: 0.6,
			AdaptiveRecover: 0.7, AdaptiveMinSamples: 10, AdaptiveRefreshSeconds: 60,
		},
	}
	fake := &fakeSamples{samples: []store.AdaptiveSample{
		{Upstream: "workbuddy", Model: "solo-model", Total: 300, OK: 10},
	}}
	a, h, now := newAdaptiveForTest(t, cfg, fake)
	a.tick(context.Background(), now)

	if f := a.factorFor("workbuddy", "solo-model"); f != 1 {
		t.Fatalf("factor = %v, want 1 (单上游模型不调整)", f)
	}
	ups := []*config.Upstream{{Name: "workbuddy", Tier: "free", Weight: 100}}
	got := h.selectCandidates(ups, "", "solo-model")
	if len(got) != 1 || got[0].Name != "workbuddy" {
		t.Fatalf("candidates = %v, want workbuddy", got)
	}
}

// 6. 降权后排序：低成功率上游确实排到后面（跑真实 selectCandidates）
func TestAdaptive_SelectCandidatesOrdering(t *testing.T) {
	cfg := adaptiveTestCfg(true, 211, 211) // 同 tier 同 weight：原本完全靠随机打乱
	fake := &fakeSamples{samples: []store.AdaptiveSample{
		{Upstream: "workbuddy", Model: "deepseek-v4.1-flash", Total: 374, OK: 93},  // 24.9%
		{Upstream: "codebuddy", Model: "deepseek-v4.1-flash", Total: 301, OK: 296}, // 98.3%
	}}
	a, h, now := newAdaptiveForTest(t, cfg, fake)
	a.tick(context.Background(), now)

	ups := []*config.Upstream{
		{Name: "workbuddy", Tier: "free", Weight: 211},
		{Name: "codebuddy", Tier: "free", Weight: 211},
	}
	// 随机打乱是随机过程，多跑几次确保稳定（真降权时任何一次都应把 workbuddy 排后）
	for i := 0; i < 30; i++ {
		got := h.selectCandidates(ups, "", "deepseek-v4.1-flash")
		if len(got) != 2 {
			t.Fatalf("candidates = %d, want 2", len(got))
		}
		if got[0].Name != "codebuddy" || got[1].Name != "workbuddy" {
			t.Fatalf("order = [%s %s], want [codebuddy workbuddy] (低成功率必须排后)",
				got[0].Name, got[1].Name)
		}
	}
}

// 7. 随机打乱分组使用有效权重（防需求书 6.2 的漏改）
// 场景：A/B 原始 weight 相同（100），A 被降权到 41、C 原始 weight = 41。
// 若打乱分组用原始 weight，B(100) 与 A(100) 会被分进同一组随机打乱，
// A 有 50% 概率被洗到 B 前面；用有效权重则 B/A/C 各成一组，顺序固定。
func TestAdaptive_ShuffleGroupingUsesEffectiveWeight(t *testing.T) {
	cfg := adaptiveTestCfg(true, 100, 100, 41)
	fake := &fakeSamples{samples: []store.AdaptiveSample{
		{Upstream: "workbuddy", Model: "deepseek-v4.1-flash", Total: 100, OK: 25}, // factor 0.4167 → 有效权重 41
		{Upstream: "codebuddy", Model: "deepseek-v4.1-flash", Total: 100, OK: 100},
		{Upstream: "third", Model: "deepseek-v4.1-flash", Total: 100, OK: 100},
	}}
	a, h, now := newAdaptiveForTest(t, cfg, fake)
	a.tick(context.Background(), now)

	ups := []*config.Upstream{
		{Name: "workbuddy", Tier: "free", Weight: 100},
		{Name: "codebuddy", Tier: "free", Weight: 100},
		{Name: "third", Tier: "free", Weight: 41},
	}
	wbEff := h.effectiveWeight(ups[0], "deepseek-v4.1-flash")
	if wbEff != 41 {
		t.Fatalf("workbuddy effective weight = %d, want 41 (前提：与 third 权重相同)", wbEff)
	}
	// codebuddy(100) 与 third(41) 权重不同，不会被打乱组混淆；
	// 关键断言：workbuddy 有效权重 41，与 codebuddy 100 不同 → 不允许被打乱洗到前面。
	for i := 0; i < 50; i++ {
		got := h.selectCandidates(ups, "", "deepseek-v4.1-flash")
		if got[0].Name != "codebuddy" {
			t.Fatalf("run %d: first = %s, want codebuddy (最高有效权重)", i, got[0].Name)
		}
		// workbuddy 有效权重 41 与 third 原始 41 相同：这两者之间打乱是允许的，
		// 但两者都必须在 codebuddy 之后。
		if got[2].Name != "workbuddy" && got[2].Name != "third" {
			t.Fatalf("run %d: last = %s, want workbuddy/third", i, got[2].Name)
		}
	}
}

// 7b. 同一降权上游不得因随机打乱出现在未降权上游之前（等价权重分组回归）
func TestAdaptive_ShuffleDoesNotPromoteDownweighted(t *testing.T) {
	cfg := adaptiveTestCfg(true, 100, 100)
	fake := &fakeSamples{samples: []store.AdaptiveSample{
		{Upstream: "workbuddy", Model: "deepseek-v4.1-flash", Total: 100, OK: 25},
		{Upstream: "codebuddy", Model: "deepseek-v4.1-flash", Total: 100, OK: 100},
	}}
	a, h, now := newAdaptiveForTest(t, cfg, fake)
	a.tick(context.Background(), now)

	ups := []*config.Upstream{
		{Name: "workbuddy", Tier: "free", Weight: 100},
		{Name: "codebuddy", Tier: "free", Weight: 100},
	}
	wbEff := h.effectiveWeight(ups[0], "deepseek-v4.1-flash")
	cbEff := h.effectiveWeight(ups[1], "deepseek-v4.1-flash")
	if wbEff >= cbEff {
		t.Fatalf("precondition: wbEff=%d cbEff=%d, want wbEff < cbEff", wbEff, cbEff)
	}
	for i := 0; i < 50; i++ {
		if got := h.selectCandidates(ups, "", "deepseek-v4.1-flash"); got[0].Name != "codebuddy" {
			t.Fatalf("run %d: first = %s, want codebuddy", i, got[0].Name)
		}
	}
}

// 8. 复权：到 probe_at 后 factor 提升到 0.3
// 用 12% 成功率（factor=0.2，低于试探系数 0.3）触发「到点进试探」分支。
func TestAdaptive_ProbeRaisesFactorToProbeLevel(t *testing.T) {
	cfg := adaptiveTestCfg(true, 211, 211)
	fake := &fakeSamples{samples: []store.AdaptiveSample{
		{Upstream: "workbuddy", Model: "deepseek-v4.1-flash", Total: 100, OK: 12},
		{Upstream: "codebuddy", Model: "deepseek-v4.1-flash", Total: 301, OK: 296},
	}}
	a, h, now := newAdaptiveForTest(t, cfg, fake)
	a.tick(context.Background(), now)

	a.mu.RLock()
	probeAt := a.entries[adaptiveKey("workbuddy", "deepseek-v4.1-flash")].probeAt
	a.mu.RUnlock()
	if !probeAt.After(now) {
		t.Fatalf("precondition: probeAt = %v, want > %v", probeAt, now)
	}

	// 未到 probe_at：保持降权。tick 必须单调推进时间（真实 ticker 就是这样），
	// 但允许在同一窗口内重复统计，因此断言「系数没有提升」而不是精确值。
	before := a.factorFor("workbuddy", "deepseek-v4.1-flash")
	if before >= adaptiveProbeFactor {
		t.Fatalf("precondition: factor = %v, want < %v（需低于试探系数才会进试探）", before, adaptiveProbeFactor)
	}

	// 到 probe_at：提升到 0.3
	a.tick(context.Background(), probeAt)
	if f := a.factorFor("workbuddy", "deepseek-v4.1-flash"); math.Abs(f-adaptiveProbeFactor) > 1e-9 {
		t.Fatalf("factor at probe = %v, want %v", f, adaptiveProbeFactor)
	}
	up := &config.Upstream{Name: "workbuddy", Tier: "free", Weight: 211}
	if got, want := h.effectiveWeight(up, "deepseek-v4.1-flash"), effWeight(211, adaptiveProbeFactor); got != want {
		t.Fatalf("effective weight = %d, want %d", got, want)
	}

	// 试探成功（试探期新流量 rate >= 0.7 且样本 >= 3）→ 完全恢复，条目删除。
	// 按新口径（TASK-probe-fix.md）判定只看试探起点之后的流量，故同时更新 newSamples。
	fake.samples[0] = store.AdaptiveSample{Upstream: "workbuddy", Model: "deepseek-v4.1-flash", Total: 5, OK: 5}
	fake.newSamples = []store.AdaptiveSample{{Upstream: "workbuddy", Model: "deepseek-v4.1-flash", Total: 5, OK: 5}}
	a.tick(context.Background(), probeAt.Add(time.Minute))
	if f := a.factorFor("workbuddy", "deepseek-v4.1-flash"); f != 1 {
		t.Fatalf("factor after successful probe = %v, want 1 (完全恢复)", f)
	}
	a.mu.RLock()
	_, exists := a.entries[adaptiveKey("workbuddy", "deepseek-v4.1-flash")]
	a.mu.RUnlock()
	if exists {
		t.Fatalf("entry 仍存在，want 已删除")
	}
}

// 8b. 试探失败 → 退避翻倍（backoff++，probe_at 后移）
func TestAdaptive_ProbeFailureBackoff(t *testing.T) {
	cfg := adaptiveTestCfg(true, 211, 211)
	fake := &fakeSamples{samples: []store.AdaptiveSample{
		{Upstream: "workbuddy", Model: "deepseek-v4.1-flash", Total: 100, OK: 12},
		{Upstream: "codebuddy", Model: "deepseek-v4.1-flash", Total: 301, OK: 296},
	}}
	a, _, now := newAdaptiveForTest(t, cfg, fake)
	a.tick(context.Background(), now)
	a.mu.RLock()
	probeAt := a.entries[adaptiveKey("workbuddy", "deepseek-v4.1-flash")].probeAt
	a.mu.RUnlock()

	a.tick(context.Background(), probeAt) // 进试探：factor → 0.3，观察期设为预测恢复时长
	a.mu.RLock()
	probeWindow := a.entries[adaptiveKey("workbuddy", "deepseek-v4.1-flash")].probeWindow
	a.mu.RUnlock()
	// 观察期内不会推进退避（只有观察期结束才判定）
	fake.samples[0] = store.AdaptiveSample{Upstream: "workbuddy", Model: "deepseek-v4.1-flash", Total: 5, OK: 0}
	fake.newSamples = []store.AdaptiveSample{{Upstream: "workbuddy", Model: "deepseek-v4.1-flash", Total: 5, OK: 0}}
	a.tick(context.Background(), probeAt.Add(time.Second))
	a.mu.RLock()
	midBackoff := a.entries[adaptiveKey("workbuddy", "deepseek-v4.1-flash")].backoff
	a.mu.RUnlock()
	if midBackoff != 0 {
		t.Fatalf("观察期内 backoff = %d, want 0", midBackoff)
	}
	// 观察期结束仍失败 → 退避
	failAt := probeAt.Add(probeWindow + time.Second)
	a.tick(context.Background(), failAt)

	a.mu.RLock()
	e := a.entries[adaptiveKey("workbuddy", "deepseek-v4.1-flash")]
	a.mu.RUnlock()
	if e == nil {
		t.Fatalf("entry 丢失")
	}
	if e.backoff != 1 {
		t.Fatalf("backoff = %d, want 1", e.backoff)
	}
	if !e.probeAt.After(failAt) {
		t.Fatalf("probeAt = %v, want > %v", e.probeAt, failAt)
	}
	if e.factor > adaptiveProbeFactor {
		t.Fatalf("factor = %v, want <= %v（试探失败回到低值）", e.factor, adaptiveProbeFactor)
	}
}

// 9. 账号池耗尽错误 → probe_at 延迟 6 小时
func TestAdaptive_QuotaExhaustedDelaysProbeSixHours(t *testing.T) {
	cfg := adaptiveTestCfg(true, 211, 211)
	now := time.Date(2026, 11, 4, 22, 0, 0, 0, time.UTC)
	// 历史里全是「账号池无可用账号」的 429
	rows := []store.AdaptiveProbeRow{
		{Status: 429, AtMS: now.Add(-5 * time.Hour).UnixMilli(), ErrText: "账号池无可用账号: 2 个账号中 2 个本次已试完"},
		{Status: 429, AtMS: now.Add(-1 * time.Hour).UnixMilli(), ErrText: "账号池无可用账号: 2 个账号中 2 个本次已试完"},
	}
	fake := &fakeSamples{
		samples: []store.AdaptiveSample{
			{Upstream: "workbuddy", Model: "deepseek-v4.1-flash", Total: 374, OK: 93},
			{Upstream: "codebuddy", Model: "deepseek-v4.1-flash", Total: 301, OK: 296},
		},
		rows: map[string][]store.AdaptiveProbeRow{adaptiveKey("workbuddy", "deepseek-v4.1-flash"): rows},
	}
	a := NewAdaptiveCache(cfg, nil, router.New(cfg))
	a.src = fake
	a.now = func() time.Time { return now }
	a.tick(context.Background(), now)

	a.mu.RLock()
	e := a.entries[adaptiveKey("workbuddy", "deepseek-v4.1-flash")]
	a.mu.RUnlock()
	if e == nil {
		t.Fatalf("未产生降权条目")
	}
	want := now.Add(adaptiveQuotaProbeDelay)
	if diff := e.probeAt.Sub(want); math.Abs(diff.Seconds()) > 1 {
		t.Fatalf("probeAt = %v, want ≈%v (额度耗尽退避 6 小时)", e.probeAt, want)
	}
}

// 9b. 瞬时 429（中位恢复 6 秒）→ probe_at 按历史故障段预测（而非固定 10 分钟）
func TestAdaptive_InstantRateLimitUsesMedianRecovery(t *testing.T) {
	cfg := adaptiveTestCfg(true, 211, 211)
	now := time.Date(2026, 11, 4, 22, 0, 0, 0, time.UTC)
	// 三段 429，每段持续 6 秒后恢复 2xx → 中位 6s
	var rows []store.AdaptiveProbeRow
	base := now.Add(-2 * time.Hour)
	for i := 0; i < 3; i++ {
		start := base.Add(time.Duration(i) * 20 * time.Minute)
		rows = append(rows,
			store.AdaptiveProbeRow{Status: 429, AtMS: start.UnixMilli()},
			store.AdaptiveProbeRow{Status: 429, AtMS: start.Add(6 * time.Second).UnixMilli()},
			store.AdaptiveProbeRow{Status: 200, AtMS: start.Add(10 * time.Second).UnixMilli()},
		)
	}
	fake := &fakeSamples{
		samples: []store.AdaptiveSample{
			{Upstream: "workbuddy", Model: "deepseek-v4.1-flash", Total: 100, OK: 25},
			{Upstream: "codebuddy", Model: "deepseek-v4.1-flash", Total: 100, OK: 100},
		},
		rows: map[string][]store.AdaptiveProbeRow{adaptiveKey("workbuddy", "deepseek-v4.1-flash"): rows},
	}
	a := NewAdaptiveCache(cfg, nil, router.New(cfg))
	a.src = fake
	a.now = func() time.Time { return now }
	a.tick(context.Background(), now)

	a.mu.RLock()
	e := a.entries[adaptiveKey("workbuddy", "deepseek-v4.1-flash")]
	a.mu.RUnlock()
	if e == nil {
		t.Fatalf("未产生降权条目")
	}
	// 6s * 1.5 = 9s
	want := now.Add(9 * time.Second)
	if diff := e.probeAt.Sub(want); math.Abs(diff.Seconds()) > 1 {
		t.Fatalf("probeAt = %v, want ≈%v (按中位恢复 6s*1.5)", e.probeAt, want)
	}
}

// 9c. 无历史故障段（<2 段）→ 默认 10 分钟 * 1.5
func TestAdaptive_NoHistoryUsesDefaultRecovery(t *testing.T) {
	cfg := adaptiveTestCfg(true, 211, 211)
	now := time.Date(2026, 11, 4, 22, 0, 0, 0, time.UTC)
	fake := &fakeSamples{samples: []store.AdaptiveSample{
		{Upstream: "workbuddy", Model: "deepseek-v4.1-flash", Total: 100, OK: 25},
		{Upstream: "codebuddy", Model: "deepseek-v4.1-flash", Total: 100, OK: 100},
	}}
	a := NewAdaptiveCache(cfg, nil, router.New(cfg))
	a.src = fake
	a.now = func() time.Time { return now }
	a.tick(context.Background(), now)

	a.mu.RLock()
	e := a.entries[adaptiveKey("workbuddy", "deepseek-v4.1-flash")]
	a.mu.RUnlock()
	want := now.Add(time.Duration(float64(adaptiveDefaultRecovery) * adaptiveProbeSafety))
	if diff := e.probeAt.Sub(want); math.Abs(diff.Seconds()) > 1 {
		t.Fatalf("probeAt = %v, want ≈%v", e.probeAt, want)
	}
}

// 10. 全部上游都低成功率时不产生空候选
func TestAdaptive_AllLowRateKeepsCandidates(t *testing.T) {
	cfg := adaptiveTestCfg(true, 100, 100, 100)
	fake := &fakeSamples{samples: []store.AdaptiveSample{
		{Upstream: "workbuddy", Model: "deepseek-v4.1-flash", Total: 100, OK: 20},
		{Upstream: "codebuddy", Model: "deepseek-v4.1-flash", Total: 100, OK: 25},
		{Upstream: "third", Model: "deepseek-v4.1-flash", Total: 100, OK: 30},
	}}
	a, h, now := newAdaptiveForTest(t, cfg, fake)
	a.tick(context.Background(), now)

	ups := []*config.Upstream{
		{Name: "workbuddy", Tier: "free", Weight: 100},
		{Name: "codebuddy", Tier: "free", Weight: 100},
		{Name: "third", Tier: "free", Weight: 100},
	}
	got := h.selectCandidates(ups, "", "deepseek-v4.1-flash")
	if len(got) != 3 {
		t.Fatalf("candidates = %d, want 3（全低也必须保留全部候选）", len(got))
	}
	// 相对关系保持：30% > 25% > 20%
	if got[0].Name != "third" || got[1].Name != "codebuddy" || got[2].Name != "workbuddy" {
		t.Fatalf("order = [%s %s %s], want [third codebuddy workbuddy]", got[0].Name, got[1].Name, got[2].Name)
	}
	// 每个有效权重都必须 >= 1
	for _, u := range ups {
		if w := h.effectiveWeight(u, "deepseek-v4.1-flash"); w < 1 {
			t.Fatalf("%s effective weight = %d, want >= 1", u.Name, w)
		}
	}
}

// 10b. tier 优先于有效权重：payg 即使权重更高也不得越过 free/subscription
func TestAdaptive_TierStillDominates(t *testing.T) {
	cfg := adaptiveTestCfg(true, 100, 100)
	fake := &fakeSamples{samples: []store.AdaptiveSample{
		{Upstream: "workbuddy", Model: "deepseek-v4.1-flash", Total: 100, OK: 20},
		{Upstream: "codebuddy", Model: "deepseek-v4.1-flash", Total: 100, OK: 100},
	}}
	a, h, now := newAdaptiveForTest(t, cfg, fake)
	a.tick(context.Background(), now)

	ups := []*config.Upstream{
		{Name: "workbuddy", Tier: "free", Weight: 100},
		{Name: "codebuddy", Tier: "payg", Weight: 10000},
	}
	got := h.selectCandidates(ups, "", "deepseek-v4.1-flash")
	if got[0].Tier != "free" {
		t.Fatalf("first tier = %s, want free（tier 优先，跨 tier 不可越过）", got[0].Tier)
	}
}

// 11. 故障段分析：末尾仍在持续的段用「现在 - 段起点」作为下限
func TestAnalyzeFaultSegments_OngoingSegmentUsesNow(t *testing.T) {
	now := time.Date(2026, 11, 4, 13, 0, 0, 0, time.UTC)
	rows := []store.AdaptiveProbeRow{
		{Status: 429, AtMS: now.Add(-9 * time.Hour).UnixMilli()},
		{Status: 429, AtMS: now.Add(-1 * time.Hour).UnixMilli()},
	}
	st := analyzeFaultSegments(rows, now)
	if st.segments != 1 {
		t.Fatalf("segments = %d, want 1", st.segments)
	}
	if st.median < 8*time.Hour || st.median > 10*time.Hour {
		t.Fatalf("median = %v, want ≈9h（持续段用 now-段起点）", st.median)
	}
}

// 12. 统计查询口径：客户端错误不出现在样本里（用真实 SQLite 校验）
func TestAdaptiveSamples_SQLFiltering(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/adaptive.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	now := time.Now().UTC()
	put := func(status int, ts time.Time) {
		if err := st.Insert(store.Record{
			Timestamp: ts, Upstream: "workbuddy", Model: "m1", Endpoint: "chat", Status: status,
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	// 成功 3、429 1、500 1、0 1 → total=6 ok=3
	for i := 0; i < 3; i++ {
		put(200, now.Add(-time.Duration(i)*time.Minute))
	}
	put(429, now.Add(-3*time.Minute))
	put(500, now.Add(-4*time.Minute))
	put(0, now.Add(-5*time.Minute))
	// 客户端错误：不计入分母
	put(400, now.Add(-6*time.Minute))
	put(401, now.Add(-7*time.Minute))
	put(403, now.Add(-8*time.Minute))
	put(404, now.Add(-9*time.Minute))
	put(422, now.Add(-10*time.Minute))
	put(499, now.Add(-11*time.Minute))
	// 窗口外：不计入
	put(200, now.Add(-10*time.Hour))

	rows, err := st.AdaptiveSamples(now.Add(-5 * time.Hour).UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("AdaptiveSamples: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1: %+v", len(rows), rows)
	}
	if rows[0].Total != 6 || rows[0].OK != 3 {
		t.Fatalf("total/ok = %d/%d, want 6/3（客户端错误与窗口外必须排除）", rows[0].Total, rows[0].OK)
	}
}

// TestAdaptive_SQLBoundaryFormat 锁死统计 SQL 的时间边界写法（TASK-window-fix.md）。
//
// 缺陷现场：下界用 datetime('now','-5 hours') 产出 '2026-11-04 10:16:37'（空格、无 Z），
// 而 request_log.ts 是 '2026-11-04T15:11:27Z'；'T'(0x54) > ' '(0x20) 使比较恒为真，
// 5 小时窗口退化成全部历史。这里直接断言传进 SQL 的 since 是 T...Z 形态。
func TestAdaptive_SQLBoundaryFormat(t *testing.T) {
	rec := &recordingSamples{}
	a := NewAdaptiveCache(nil, nil, nil)
	a.src = rec
	a.windowHours = 5
	now := time.Date(2026, 11, 4, 22, 56, 0, 0, time.UTC)
	a.tick(context.Background(), now)

	if got, want := rec.since, "2026-11-04T17:56:00Z"; got != want {
		t.Fatalf("窗口下界传给 SQL 的是 %q，want %q（UTC RFC3339，带 T 带 Z）", got, want)
	}
	if t10 := rec.since[10]; t10 != 'T' {
		t.Fatalf("窗口下界 %q 第 11 字节是 %q，必须为 'T'（否则与 ts 字符串比较失效）", rec.since, t10)
	}

	// 当天口径：东八区 00:00 = UTC 前一日 16:00
	rec.since = ""
	a.windowHours = 0
	a.tick(context.Background(), now)
	if got, want := rec.since, "2026-11-04T16:00:00Z"; got != want {
		t.Fatalf("当天窗口下界 = %q, want %q（东八区 00:00）", got, want)
	}
}

// recordingSamples 记录最后一次收到的 since，其余返回空结果。
type recordingSamples struct{ since string }

func (r *recordingSamples) AdaptiveSamples(since string) ([]store.AdaptiveSample, error) {
	r.since = since
	return nil, nil
}

func (r *recordingSamples) AdaptiveSamplesSince(since, _ string) ([]store.AdaptiveSample, error) {
	r.since = since
	return nil, nil
}

func (r *recordingSamples) AdaptiveProbeRows(string, string, string, int) ([]store.AdaptiveProbeRow, error) {
	return nil, nil
}

// 13. 持久化往返：降权写库、恢复删行（启动后可恢复，不因重启归零）
func TestAdaptive_PersistAndRecover(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/adaptive.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	cfg := adaptiveTestCfg(true, 211, 211)
	now := time.Date(2026, 11, 4, 22, 0, 0, 0, time.UTC)
	fake := &fakeSamples{samples: []store.AdaptiveSample{
		{Upstream: "workbuddy", Model: "deepseek-v4.1-flash", Total: 374, OK: 93},
		{Upstream: "codebuddy", Model: "deepseek-v4.1-flash", Total: 301, OK: 296},
	}}
	a := NewAdaptiveCache(cfg, st, router.New(cfg))
	a.src = fake
	a.now = func() time.Time { return now }
	a.tick(context.Background(), now)

	rows, err := st.ListAdaptiveWeights()
	if err != nil {
		t.Fatalf("ListAdaptiveWeights: %v", err)
	}
	if len(rows) != 1 || rows[0].Upstream != "workbuddy" {
		t.Fatalf("persisted rows = %+v, want 1 row for workbuddy", rows)
	}

	// 新实例 Load 后系数仍在（模拟重启）
	b := NewAdaptiveCache(cfg, st, router.New(cfg))
	b.Load()
	b.mu.RLock()
	e := b.entries[adaptiveKey("workbuddy", "deepseek-v4.1-flash")]
	b.mu.RUnlock()
	if e == nil || math.Abs(e.factor-rows[0].Factor) > 1e-9 {
		t.Fatalf("loaded entry = %+v, want factor %v", e, rows[0].Factor)
	}

	// 恢复后 DB 行被删除
	a.recoverEntry(*e, now)
	rows, err = st.ListAdaptiveWeights()
	if err != nil {
		t.Fatalf("ListAdaptiveWeights: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows after recover = %d, want 0", len(rows))
	}
}

// 14. 开关关闭时不启动后台 goroutine 也不读库
func TestAdaptive_StartNoopWhenDisabled(t *testing.T) {
	cfg := adaptiveTestCfg(false, 100, 100)
	counted := &countingSamples{}
	a := NewAdaptiveCache(cfg, nil, router.New(cfg))
	a.src = counted
	stop := a.Start(context.Background())
	time.Sleep(50 * time.Millisecond)
	stop()
	stop() // 幂等
	if n := counted.calls(); n != 0 {
		t.Fatalf("samples 调用 %d 次，want 0（开关关闭必须零查询）", n)
	}
	if snap := a.Snapshot(); len(snap) != 0 {
		t.Fatalf("snapshot = %v, want empty", snap)
	}
}

type countingSamples struct {
	n int
}

func (c *countingSamples) calls() int { return c.n }

func (c *countingSamples) AdaptiveSamples(string) ([]store.AdaptiveSample, error) {
	c.n++
	return nil, nil
}

func (c *countingSamples) AdaptiveSamplesSince(string, string) ([]store.AdaptiveSample, error) {
	c.n++
	return nil, nil
}

func (c *countingSamples) AdaptiveProbeRows(string, string, string, int) ([]store.AdaptiveProbeRow, error) {
	c.n++
	return nil, nil
}

// 15. EffectiveWeight 对 nil/未知条目安全，且下限为 1
func TestAdaptive_EffectiveWeightBounds(t *testing.T) {
	cfg := adaptiveTestCfg(true, 1, 1)
	a := NewAdaptiveCache(cfg, nil, router.New(cfg))
	if got := a.EffectiveWeight(nil, "m"); got != 0 {
		t.Fatalf("nil upstream = %d, want 0", got)
	}
	a.mu.Lock()
	a.entries[adaptiveKey("workbuddy", "m")] = &adaptiveEntry{upstream: "workbuddy", model: "m", factor: 0.05}
	a.mu.Unlock()
	if got := a.EffectiveWeight(&config.Upstream{Name: "workbuddy", Weight: 1}, "m"); got != 1 {
		t.Fatalf("EffectiveWeight = %d, want 1（下限 1）", got)
	}
}

// 16. 统计窗口：window_hours=0 表示「当天」（东八区 00:00）
func TestAdaptive_WindowSince(t *testing.T) {
	a := NewAdaptiveCache(nil, nil, nil)
	a.windowHours = 5
	now := time.Date(2026, 11, 4, 22, 56, 0, 0, time.UTC)
	if got, want := a.windowSince(now), now.Add(-5*time.Hour).UTC().Format(time.RFC3339); got != want {
		t.Fatalf("windowSince = %s, want %s", got, want)
	}
	// 「当天」按东八区：2026-11-04T22:56Z 在东八区已是 11-05 06:56，
	// 故当天零点应为北京时间 11-05 00:00 = UTC 11-04T16:00Z。
	a.windowHours = 0
	const want = "2026-11-04T16:00:00Z"
	if got := a.windowSince(now); got != want {
		t.Fatalf("windowSince(0) = %s, want %s（东八区当天 00:00）", got, want)
	}
	// now 落在东八区凌晨（UTC 前一天 17:00）时也必须算当天，不能落到 UTC 日界。
	early := time.Date(2026, 11, 4, 17, 30, 0, 0, time.UTC) // 北京 11-05 01:30
	if got, want := a.windowSince(early), "2026-11-04T16:00:00Z"; got != want {
		t.Fatalf("windowSince(0)@北京凌晨 = %s, want %s", got, want)
	}
	// 与 now 的 Location 无关（进程跑在 UTC 时不能算成 UTC 零点）
	a2 := NewAdaptiveCache(nil, nil, nil)
	a2.windowHours = 0
	if got, want := a2.windowSince(now.In(time.UTC)), "2026-11-04T16:00:00Z"; got != want {
		t.Fatalf("windowSince(0)@UTC时钟 = %s, want %s", got, want)
	}
}

// --- 试探期口径回归（TASK-probe-fix.md）-----------------------------------------
//
// 缺陷现场：历史窗口里压着 291 条 429，试探期放出的 10 次流量全成功，
// 用全窗口成功率算成 10/291=3.4% < 恢复阈 70% → 判「试探失败」→ 系数压回低值，
// 被降权上游永远走不出「降权 → 试探 → 失败 → 再降权」的循环。

// probeScenario 构造一个「已被降权、正在试探」的现场：
//
//   - 历史 291 条全失败（试探起点之前）；
//   - 试探起点之后新增 newOK/newFail 条流量；
//   - 返回 cache、fake 与试探观察期结束时刻。
func probeScenario(t *testing.T, newOK, newFail int) (*AdaptiveCache, *fakeSamples, time.Time) {
	t.Helper()
	cfg := adaptiveTestCfg(true, 211, 211)
	const model = "deepseek-v4.1-flash"
	wb := adaptiveKey("workbuddy", model)
	// 先只有历史样本：291 条全失败（1.0% 左右，远低于降权阈）
	fake := &fakeSamples{
		samples: []store.AdaptiveSample{
			{Upstream: "workbuddy", Model: model, Total: 291, OK: 0},
			{Upstream: "codebuddy", Model: model, Total: 301, OK: 296},
		},
	}
	a := NewAdaptiveCache(cfg, nil, router.New(cfg))
	a.src = fake
	now := time.Date(2026, 11, 4, 22, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	a.tick(context.Background(), now)

	a.mu.RLock()
	e := a.entries[wb]
	a.mu.RUnlock()
	if e == nil || e.factor >= adaptiveProbeFactor {
		t.Fatalf("precondition: 需要先被降权到 <%v，实际 %+v", adaptiveProbeFactor, e)
	}
	probeAt := e.probeAt

	// 到 probe_at 进试探（记录试探起点）
	a.tick(context.Background(), probeAt)
	a.mu.RLock()
	cur := a.entries[wb]
	a.mu.RUnlock()
	if cur == nil || math.Abs(cur.factor-adaptiveProbeFactor) > 1e-9 {
		t.Fatalf("precondition: 应已进入试探，factor = %v", cur)
	}
	if cur.probeStart.IsZero() {
		t.Fatalf("precondition: 进入试探必须记录 probeStart")
	}

	// 试探起点之后新增流量：全窗口样本仍是「291 失败 + 新流量」，
	// 但试探口径（AdaptiveSamplesSince(since, probeStart)）只能看到新增部分。
	fake.newSamples = []store.AdaptiveSample{
		{Upstream: "workbuddy", Model: model, Total: newOK + newFail, OK: newOK},
	}
	return a, fake, cur.probeAt
}

// 17. 试探期新流量全成功 → 完全恢复（条目被删除、factor 回到 1.0）
func TestAdaptive_ProbeRecoversWhenNewTrafficHealthy(t *testing.T) {
	a, _, probeEnd := probeScenario(t, 10, 0)
	wb := adaptiveKey("workbuddy", "deepseek-v4.1-flash")

	// 反证：若沿用全窗口口径（历史 291 失败 + 新 10 成功 = 3.3%），必然判失败。
	fullTotal := 291 + 10
	if float64(10)/float64(fullTotal) >= a.recover {
		t.Fatalf("precondition: 全窗口成功率不应达到恢复阈，否则无法证明修复生效")
	}

	// 观察期结束：只看 probeStart 之后的 10 条（全成功）→ 完全恢复
	a.tick(context.Background(), probeEnd.Add(time.Second))

	a.mu.RLock()
	_, exists := a.entries[wb]
	a.mu.RUnlock()
	if exists {
		t.Fatalf("试探期新流量全成功，应完全恢复（条目删除），实际 factor = %v",
			a.factorFor("workbuddy", "deepseek-v4.1-flash"))
	}
	if f := a.factorFor("workbuddy", "deepseek-v4.1-flash"); f != 1 {
		t.Fatalf("factor = %v, want 1（完全恢复）", f)
	}
	up := &config.Upstream{Name: "workbuddy", Tier: "free", Weight: 211}
	if got := a.EffectiveWeight(up, "deepseek-v4.1-flash"); got != 211 {
		t.Fatalf("EffectiveWeight = %d, want 211", got)
	}
}

// 18. 试探期新流量全失败 → 退避（backoff 递增，probeStart 清零）
func TestAdaptive_ProbeFailsWhenNewTrafficBad(t *testing.T) {
	a, _, probeEnd := probeScenario(t, 0, 10)
	wb := adaptiveKey("workbuddy", "deepseek-v4.1-flash")

	a.tick(context.Background(), probeEnd.Add(time.Second))

	a.mu.RLock()
	e := a.entries[wb]
	a.mu.RUnlock()
	if e == nil {
		t.Fatalf("试探失败不应完全恢复（条目丢失）")
	}
	if e.backoff != 1 {
		t.Fatalf("backoff = %d, want 1", e.backoff)
	}
	if !e.probeStart.IsZero() {
		t.Fatalf("probeStart = %v, want 零值（已退出试探期）", e.probeStart)
	}
	if e.factor > adaptiveProbeFactor {
		t.Fatalf("factor = %v, want <= %v（退避回到低值）", e.factor, adaptiveProbeFactor)
	}
	if e.samples != 10 {
		t.Fatalf("samples = %d, want 10（只统计试探起点之后的流量）", e.samples)
	}
	if !e.probeAt.After(probeEnd) {
		t.Fatalf("probeAt = %v, want > %v", e.probeAt, probeEnd)
	}
}

// 19. 观察期内样本不足 → 不退避，保持试探系数并顺延观察期
func TestAdaptive_ProbeExtendsWhenSamplesInsufficient(t *testing.T) {
	// 历史 291 条失败，试探起点后只来 1 条（成功）——够不上 adaptiveProbeMinSamples
	a, _, probeEnd := probeScenario(t, 1, 0)
	wb := adaptiveKey("workbuddy", "deepseek-v4.1-flash")

	a.tick(context.Background(), probeEnd.Add(time.Second))

	a.mu.RLock()
	e := a.entries[wb]
	a.mu.RUnlock()
	if e == nil {
		t.Fatalf("条目丢失")
	}
	if e.backoff != 0 {
		t.Fatalf("backoff = %d, want 0（样本不足不得退避）", e.backoff)
	}
	if math.Abs(e.factor-adaptiveProbeFactor) > 1e-9 {
		t.Fatalf("factor = %v, want %v（保持试探系数继续观察）", e.factor, adaptiveProbeFactor)
	}
	if e.probeStart.IsZero() {
		t.Fatalf("仍在试探期，probeStart 不应清零")
	}
	if !e.probeAt.After(probeEnd) {
		t.Fatalf("probeAt = %v, want > %v（观察期已顺延）", e.probeAt, probeEnd)
	}
}

// 20. probe_start 持久化：重启后试探期不丢起点
func TestAdaptive_ProbeStartPersisted(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/adaptive.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	cfg := adaptiveTestCfg(true, 211, 211)
	now := time.Date(2026, 11, 4, 22, 0, 0, 0, time.UTC)
	fake := &fakeSamples{samples: []store.AdaptiveSample{
		{Upstream: "workbuddy", Model: "deepseek-v4.1-flash", Total: 100, OK: 12},
		{Upstream: "codebuddy", Model: "deepseek-v4.1-flash", Total: 100, OK: 100},
	}}
	a := NewAdaptiveCache(cfg, st, router.New(cfg))
	a.src = fake
	a.now = func() time.Time { return now }
	a.tick(context.Background(), now)
	a.mu.RLock()
	probeAt := a.entries[adaptiveKey("workbuddy", "deepseek-v4.1-flash")].probeAt
	a.mu.RUnlock()

	a.tick(context.Background(), probeAt) // 进试探 → probeStart 落库
	rows, err := st.ListAdaptiveWeights()
	if err != nil {
		t.Fatalf("ListAdaptiveWeights: %v", err)
	}
	if len(rows) != 1 || rows[0].ProbeStart == "" {
		t.Fatalf("rows = %+v, want probe_start 已落库", rows)
	}

	b := NewAdaptiveCache(cfg, st, router.New(cfg))
	b.Load()
	b.mu.RLock()
	e := b.entries[adaptiveKey("workbuddy", "deepseek-v4.1-flash")]
	b.mu.RUnlock()
	if e == nil || !e.inProbe() {
		t.Fatalf("loaded entry = %+v, want 仍在试探期（probeStart 恢复）", e)
	}
	if got := formatAdaptiveTime(e.probeStart); got != rows[0].ProbeStart {
		t.Fatalf("probeStart = %s, want %s", got, rows[0].ProbeStart)
	}
}
