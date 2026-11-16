package admin

import (
	"testing"

	"github.com/icefairy/xuanji/internal/store"
)

// TestExistingUpstreamSet 验证：统计过滤依据（现存上游集合）只含
// "上游管理中仍存在"的上游，已删除的僵尸上游不在集合内。
// 这样 MetricsUpstreams 遍历 agg.ByUpstream 时就能跳过僵尸上游。
func TestExistingUpstreamSet(t *testing.T) {
	cfg := testConfig() // 含 硅基流动 / opencode_go / vllm-local

	// 内存 sqlite：只插入"现存"上游（DB 化后的权威来源）
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	// 现存上游：来自 cfg 的 3 个 + 一个仅 DB 存在的上游
	for _, name := range []string{"硅基流动", "opencode_go", "vllm-local", "db-only-up"} {
		if err := s.CreateUpstream(&store.UpstreamRow{Name: name, Type: "openai", Enabled: 1}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	h, _ := newTestHandler(t, cfg)
	h.SetStore(s)

	set := h.existingUpstreamSet()

	// 现存上游必须在集合内（cfg ∪ db）
	for _, want := range []string{"硅基流动", "opencode_go", "vllm-local", "db-only-up"} {
		if !set[want] {
			t.Errorf("现存上游 %q 应存在于集合，但不在", want)
		}
	}

	// 僵尸上游（已在上游管理中删除，仅 request_log 留痕）必须不在集合内
	for _, zombie := range []string{"gptest", "商汤-cyp", "商汤-kh", "modelscope", "spark-proxy"} {
		if set[zombie] {
			t.Errorf("僵尸上游 %q 不应存在于集合，但在", zombie)
		}
	}
}
