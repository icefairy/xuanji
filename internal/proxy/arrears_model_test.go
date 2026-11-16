package proxy

import (
	"strings"
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

// TestMarkArrear_ReasonCoversTopLevelMessage 回归测试（2026-09-26 实测）：
// 欠费原因提取不能只认 `error.message`，否则「顶层 message」形态的上游
// （如基元律动 `{"code":"INSUFFICIENT_BALANCE","message":"余额不足","traceId":...}`）
// 会得到空 reason：
//
//	日志：upstream arrear detected, marking upstream=基元律动gx model=glm-5.3-flash resp_body=""
//	而同一请求的 4xx/5xx detail 里明明有：body={"code":"INSUFFICIENT_BALANCE","message":"余额不足",...}
//
// 危害：
//   - 排障时看到 resp_body="" 无法判断判定依据（是否真的余额不足？还是误判？）
//   - 模型级欠费把 reason 写入 upstream_model_arrears.reason，该字段为空会污染数据，
//     管理页看不到「为什么被判欠费」
//
// 此处断言 reason 非空且包含上游给出的说明。
func TestMarkArrear_ReasonCoversTopLevelMessage(t *testing.T) {
	// 上游级：捕获日志不便，直接验证传给模型级回调的 reason（同一 reason 变量）
	var gotReason string
	h := newStrategyTestHandler()
	h.SetArrearsModelMarker(func(upstream, model, reason string) { gotReason = reason })

	// 基元律动形态：顶层 message（无 error 包装）
	body := []byte(`{"code":"INSUFFICIENT_BALANCE","message":"余额不足","traceId":"trace_x"}`)
	h.markArrear(&config.Upstream{Name: "u-permodel", PerModelBilling: true}, "m1", body)

	if strings.TrimSpace(gotReason) == "" {
		t.Errorf("reason 为空 —— 顶层 message 形态的欠费原因丢失（实测 resp_body=\"\"）；body=%s", body)
	}
	if !strings.Contains(gotReason, "余额不足") {
		t.Errorf("reason = %q, want 包含「余额不足」（上游给出的说明）", gotReason)
	}

	// 反向保护：OpenAI 风格 error.message 仍要正常提取
	gotReason = ""
	h.markArrear(&config.Upstream{Name: "u-permodel", PerModelBilling: true}, "m2",
		[]byte(`{"error":{"message":"Allocated quota exceeded","type":"insufficient_quota"}}`))
	if !strings.Contains(gotReason, "Allocated quota exceeded") {
		t.Errorf("error.message 形态的 reason = %q, want 含 Allocated quota exceeded", gotReason)
	}
}
