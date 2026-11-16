package proxy

import (
	"testing"

	"github.com/icefairy/xuanji/internal/config"
)

// 欠费上游必须被硬排除：即使全部候选都欠费，也返回空列表（宁报错不打欠费渠道）。
// 这是与 enabled/health 过滤的关键区别——后者在全部被过滤时保留原列表兜底。
func TestSelectCandidates_ArrearsHardExclude(t *testing.T) {
	h := newStrategyTestHandler()
	ups := []*config.Upstream{
		{Name: "ok-free", Tier: "free", Weight: 100, Enabled: true},
		{Name: "arrear-payg", Tier: "payg", Weight: 1000, Enabled: true, Arrears: true},
	}
	got := h.selectCandidates(ups, "", "m1")
	if len(got) != 1 || got[0].Name != "ok-free" {
		t.Fatalf("candidates = %v, want [ok-free] (欠费 payg 高权重也不该被选)", arrearTestNames(got))
	}

	// 全部欠费：返回空，绝不 fallback
	allArrear := []*config.Upstream{
		{Name: "a1", Tier: "free", Weight: 100, Enabled: true, Arrears: true},
		{Name: "a2", Tier: "free", Weight: 200, Enabled: true, Arrears: true},
	}
	if got := h.selectCandidates(allArrear, "", "m1"); len(got) != 0 {
		t.Errorf("all arrear: got %v, want empty", arrearTestNames(got))
	}
}

func arrearTestNames(ups []*config.Upstream) []string {
	out := make([]string, 0, len(ups))
	for _, u := range ups {
		out = append(out, u.Name)
	}
	return out
}
