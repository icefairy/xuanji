package health

import (
	"testing"
	"time"

	"github.com/icefairy/xuanji/internal/config"
)

func TestLatency_Unknown(t *testing.T) {
	c := New(testCfg())
	defer c.Close()
	if got := c.Latency("ghost"); got != 0 {
		t.Errorf("Latency(ghost) = %v, want 0", got)
	}
}

func TestSortByLatency(t *testing.T) {
	up1 := &config.Upstream{Name: "slow"}
	up2 := &config.Upstream{Name: "fast"}
	up3 := &config.Upstream{Name: "untested"}

	cfg := &config.Config{
		Upstreams: []config.Upstream{*up1, *up2, *up3},
	}
	c := New(cfg)
	defer c.Close()

	// 手动设置延迟（通过状态字段直接操作不现实，这里用 MockLatency 风格的测试需要内部访问，
	// 简化：通过真实健康检查不可行，改为直接断言 Latency 初始为 0，然后验证排序的稳定行为）
	c.mu.Lock()
	c.states["slow"].latency = 300 * time.Millisecond
	c.states["fast"].latency = 50 * time.Millisecond
	// untested 保持 0
	c.mu.Unlock()

	ups := []*config.Upstream{up1, up2, up3}
	got := c.SortByLatency(ups)
	wantOrder := []string{"fast", "slow", "untested"}
	for i, want := range wantOrder {
		if got[i].Name != want {
			t.Errorf("SortByLatency[%d] = %q, want %q", i, got[i].Name, want)
		}
	}
	// 原切片不被修改
	if ups[0].Name != "slow" {
		t.Errorf("original slice modified: ups[0] = %q", ups[0].Name)
	}
}
