package proxy

import (
	"testing"

	"github.com/icefairy/xuanji/internal/config"
)

// per_model_billing 上游：模型 A 欠费只跳过 A，同上游模型 B 继续可用。
// 关闭开关的上游：任一模型欠费整条停（现有语义不受影响）。
func TestSelectCandidates_ModelArrearsIsolation(t *testing.T) {
	h := newStrategyTestHandler()
	ups := []*config.Upstream{
		{Name: "permodel-up", Tier: "free", Weight: 100, Enabled: true, PerModelBilling: true},
	}
	// 模型 a 欠费：请求 a 无候选；请求 b 不受影响
	h.markModelArrear("permodel-up", "a", "test")
	if got := h.selectCandidates(ups, "", "a"); len(got) != 0 {
		t.Errorf("model a: candidates = %v, want empty (该模型欠费)", arrearTestNames(got))
	}
	if got := h.selectCandidates(ups, "", "b"); len(got) != 1 {
		t.Errorf("model b: candidates = %v, want [permodel-up] (其它模型不受影响)", arrearTestNames(got))
	}

	// 清除后恢复
	h.ClearModelArrearCache("permodel-up", "a")
	if got := h.selectCandidates(ups, "", "a"); len(got) != 1 {
		t.Errorf("after clear: candidates = %v, want [permodel-up]", arrearTestNames(got))
	}

	// 非 per_model_billing 上游走上游级 Arrears 字段，模型级缓存不影响路由
	plain := []*config.Upstream{{Name: "plain-up", Tier: "free", Weight: 100, Enabled: true}}
	h.markModelArrear("plain-up", "x", "test")
	if got := h.selectCandidates(plain, "", "x"); len(got) != 1 {
		t.Errorf("plain upstream model-level mark must NOT route-filter: %v", arrearTestNames(got))
	}
}

// markArrear 按 PerModelBilling 分派粒度（回调注入验证）。
func TestMarkArrear_Granularity(t *testing.T) {
	var upHits, modelHits []string
	newHandler := func() *Handler {
		h := newStrategyTestHandler()
		h.SetArrearsMarker(func(name string) { upHits = append(upHits, name) })
		h.SetArrearsModelMarker(func(upstream, model, reason string) {
			modelHits = append(modelHits, upstream+"::"+model)
		})
		return h
	}
	body := []byte(`{"error":{"message":"Allocated quota exceeded"}}`)

	// 上游级：只打上游回调
	h := newHandler()
	h.markArrear(&config.Upstream{Name: "u-whole"}, "m1", body)
	if len(upHits) != 1 || upHits[0] != "u-whole" || len(modelHits) != 0 {
		t.Errorf("upstream-level: upHits=%v modelHits=%v", upHits, modelHits)
	}

	// 模型级：只打模型回调
	upHits, modelHits = nil, nil
	h2 := newHandler()
	h2.markArrear(&config.Upstream{Name: "u-permodel", PerModelBilling: true}, "m2", body)
	if len(modelHits) != 1 || modelHits[0] != "u-permodel::m2" || len(upHits) != 0 {
		t.Errorf("model-level: upHits=%v modelHits=%v", upHits, modelHits)
	}
	if !h2.IsModelArrear("u-permodel", "m2") || h2.IsModelArrear("u-permodel", "m3") {
		t.Error("IsModelArrear: m2 should be marked, m3 not")
	}
}
