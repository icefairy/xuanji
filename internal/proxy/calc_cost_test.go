package proxy

import "testing"

// 费用计算：有缓存统计时按 命中价×hit + 未命中价×miss + 输出价×completion。
func TestCalcCost_WithCacheStats(t *testing.T) {
	h := &Handler{}
	h.priceFor = func(model string) (input, cache, out float64, ok bool) {
		if model == "deepseek-v4-flash-0731" {
			return 1.0, 0.1, 2.0, true
		}
		return 0, 0, 0, false
	}
	// 1000 输入：600 命中 + 400 未命中；200 输出
	// cost = 400/1e6*1.0 + 600/1e6*0.1 + 200/1e6*2.0 = 0.0004 + 0.00006 + 0.0004
	cost := h.calcCost("deepseek-v4-flash-0731", "novel", 1000, 200, 600, 400)
	want := 400.0/1e6*1.0 + 600.0/1e6*0.1 + 200.0/1e6*2.0
	if diff := cost - want; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("有缓存统计计费错误: cost=%v (期望 %v)", cost, want)
	}
}

// 无缓存统计（hit=0 miss=0 且输入>0）：输入按未命中价全额计费，不得白嫖。
func TestCalcCost_NoCacheStats_FallsBackToMissPrice(t *testing.T) {
	h := &Handler{}
	h.priceFor = func(model string) (input, cache, out float64, ok bool) {
		if model == "deepseek-v4-flash-0731" {
			return 1.0, 0.1, 2.0, true
		}
		return 0, 0, 0, false
	}
	// 上游没报缓存命中：1000 输入全部按未命中价 1.0/百万
	cost := h.calcCost("deepseek-v4-flash-0731", "novel", 1000, 200, 0, 0)
	want := 1000.0/1e6*1.0 + 200.0/1e6*2.0
	if cost != want {
		t.Fatalf("无缓存统计兜底错误: cost=%v (期望 %v)", cost, want)
	}
}

// 无价格表配置：返回 0 不计费。
func TestCalcCost_NoPriceTable(t *testing.T) {
	h := &Handler{} // priceFor 为 nil
	if cost := h.calcCost("m", "m", 100, 50, 0, 0); cost != 0 {
		t.Fatalf("无价格表应返回 0: got %v", cost)
	}
}
