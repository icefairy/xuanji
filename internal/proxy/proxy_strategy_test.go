package proxy

import (
	"testing"
	"time"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/health"
	"github.com/icefairy/xuanji/internal/router"
	"github.com/icefairy/xuanji/internal/store"
)

func testCfgForStrategy() *config.Config {
	return &config.Config{
		Routing: config.Routing{DefaultStrategy: "primary_backup"},
		Upstreams: []config.Upstream{
			{Name: "free-a", Tier: "free", Priority: 1, Weight: 100, Models: []string{"m1"}},
			{Name: "free-b", Tier: "free", Priority: 2, Weight: 100, Models: []string{"m1"}},
			{Name: "payg-c", Tier: "payg", Priority: 1, Weight: 100, Models: []string{"m1"}},
		},
	}
}

func newStrategyTestHandler() *Handler {
	cfg := testCfgForStrategy()
	rt := router.New(cfg)
	hc := health.New(cfg)
	return New(cfg, rt, hc)
}

// 统一优先级下 tier 必须优先：subscription(包月) > free > payg（payg 永远最后）
func TestSelectCandidates_TierAlwaysFirst(t *testing.T) {
	h := newStrategyTestHandler()
	ups := []*config.Upstream{
		{Name: "payg-c", Tier: "payg", Priority: 1, Weight: 1000},
		{Name: "free-a", Tier: "free", Priority: 1, Weight: 1},
		{Name: "sub-b", Tier: "subscription", Priority: 2, Weight: 1},
	}
	for _, strategy := range []string{"", "primary_backup", "weighted", "latency", "quota"} {
		got := h.selectCandidates(ups, strategy, "m1")
		if got[0].Name != "sub-b" {
			t.Errorf("strategy=%s: first = %s, want subscription tier upstream (包月优先)", strategy, got[0].Name)
		}
		if got[1].Name != "free-a" {
			t.Errorf("strategy=%s: second = %s, want free tier upstream", strategy, got[1].Name)
		}
		if got[2].Name != "payg-c" {
			t.Errorf("strategy=%s: last = %s, want payg-c", strategy, got[2].Name)
		}
	}
}

// 同 tier 内按 weight 降序（权重高优先）——统一优先级第二级
func TestSelectCandidates_WeightOrder(t *testing.T) {
	h := newStrategyTestHandler()
	ups := []*config.Upstream{
		{Name: "free-b", Tier: "free", Weight: 50},
		{Name: "free-a", Tier: "free", Weight: 200},
		{Name: "free-c", Tier: "free", Weight: 100},
	}
	got := h.selectCandidates(ups, "", "m1")
	if got[0].Name != "free-a" || got[1].Name != "free-c" || got[2].Name != "free-b" {
		t.Errorf("order = %v, want [free-a free-c free-b] (weight 降序)", []string{got[0].Name, got[1].Name, got[2].Name})
	}
}

// 同 weight 同折扣同延迟：随机打乱（不再保持原序）
func TestSelectCandidates_SameWeightShuffle(t *testing.T) {
	h := newStrategyTestHandler()
	ups := []*config.Upstream{
		{Name: "free-b", Tier: "free", Priority: 2, Weight: 100},
		{Name: "free-a", Tier: "free", Priority: 1, Weight: 100},
	}
	// 跑 50 次，两个上游都应出现过——证明不是固定原序
	var seenA, seenB bool
	for i := 0; i < 50; i++ {
		got := h.selectCandidates(ups, "", "m1")
		if got[0].Name == "free-a" {
			seenA = true
		} else {
			seenB = true
		}
		if seenA && seenB {
			break
		}
	}
	if !seenA || !seenB {
		t.Errorf("同 weight 上游应随机打乱，但 50 次只见到固定顺序 (seenA=%v seenB=%v)", seenA, seenB)
	}
}

// 同 weight 内：处于优惠时段的上游优先（统一优先级第三级）
func TestSelectCandidates_DiscountFirst(t *testing.T) {
	h := newStrategyTestHandler()
	ups := []*config.Upstream{
		{Name: "free-a", Tier: "free", Weight: 100},
		{Name: "free-b", Tier: "free", Weight: 100},
	}
	// 给 free-b 配一个当前生效的优惠时段（全天）
	h.SetDiscounts([]store.Discount{
		{Upstream: "free-b", ModelPattern: "*", StartTime: "00:00", EndTime: "23:59", Discount: 0.5},
	})
	got := h.selectCandidates(ups, "", "m1")
	if got[0].Name != "free-b" || got[1].Name != "free-a" {
		t.Errorf("order = %v, want [free-b free-a] (优惠时段优先)", []string{got[0].Name, got[1].Name})
	}
}

// 禁用上游不参与候选（enabled=false 被过滤）
func TestSelectCandidates_DisabledExcluded(t *testing.T) {
	h := newStrategyTestHandler()
	ups := []*config.Upstream{
		{Name: "free-a", Tier: "free", Weight: 100, Enabled: false},
		{Name: "free-b", Tier: "free", Weight: 100, Enabled: true},
	}
	got := h.selectCandidates(ups, "", "m1")
	if len(got) != 1 || got[0].Name != "free-b" {
		t.Errorf("candidates = %v, want [free-b] only (disabled excluded)", names(got))
	}
}

// 同 weight 同折扣状态：延迟低的优先（统一优先级第四级）；未测过延迟（0）排最后
func TestSelectCandidates_LatencyFirst(t *testing.T) {
	h := newStrategyTestHandler()
	ups := []*config.Upstream{
		{Name: "free-a", Tier: "free", Weight: 100},
		{Name: "free-b", Tier: "free", Weight: 100},
		{Name: "payg-c", Tier: "payg", Weight: 100},
	}
	// 手动设置延迟（名字必须在 health states 中，故用 cfg 里的 free-a/free-b）：
	// free-a 200ms，free-b 1500ms，payg-c 未测（0）
	h.health.SetLatencyForTest("free-a", 200*time.Millisecond)
	h.health.SetLatencyForTest("free-b", 1500*time.Millisecond)
	got := h.selectCandidates(ups, "", "m1")
	if got[0].Name != "free-a" {
		t.Errorf("first = %s, want free-a (延迟低优先)", got[0].Name)
	}
	if got[1].Name != "free-b" {
		t.Errorf("second = %s, want free-b", got[1].Name)
	}
	if got[2].Name != "payg-c" {
		t.Errorf("last = %s, want payg-c (tier 保护: payg 永远最后)", got[2].Name)
	}
}

// 延迟只在同 weight 内起作用：weight 高的即使延迟高也优先于 weight 低的
func TestSelectCandidates_LatencyDoesNotOverrideWeight(t *testing.T) {
	h := newStrategyTestHandler()
	ups := []*config.Upstream{
		{Name: "free-a", Tier: "free", Weight: 100},
		{Name: "free-b", Tier: "free", Weight: 500},
	}
	h.health.SetLatencyForTest("free-a", 50*time.Millisecond)
	h.health.SetLatencyForTest("free-b", 2000*time.Millisecond)
	got := h.selectCandidates(ups, "", "m1")
	if got[0].Name != "free-b" {
		t.Errorf("first = %s, want free-b (weight 优先于延迟)", got[0].Name)
	}
}

func names(ups []*config.Upstream) []string {
	var out []string
	for _, u := range ups {
		out = append(out, u.Name)
	}
	return out
}

var _ = health.New // 保留 health 依赖引用（SelectCandidates 健康过滤在真实链路生效）
