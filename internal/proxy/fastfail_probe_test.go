package proxy

import (
	"testing"
	"time"
)

// TestFastFail_RecoverViaSuccess 验证：真实请求成功后 MarkSuccess 解除黑名单（懒加载恢复路径）。
func TestFastFail_RecoverViaSuccess(t *testing.T) {
	ff := NewFastFailCache(time.Hour)
	ff.MarkFailed("up", "deepseek-v4-flash")
	if !ff.IsBlacklisted("up", "deepseek-v4-flash") {
		t.Fatal("should be blacklisted after MarkFailed")
	}
	ff.MarkSuccess("up", "deepseek-v4-flash")
	if ff.IsBlacklisted("up", "deepseek-v4-flash") {
		t.Error("upstream should be recovered after MarkSuccess")
	}
}

// TestFastFail_CleanupExpired 验证：懒加载 Cleanup 只清理冷却到期的条目，
// 未到期条目保留（到期即放行，不主动探测）。
func TestFastFail_CleanupExpired(t *testing.T) {
	ff := NewFastFailCache(time.Hour)
	ff.MarkFailed("a", "")
	ff.MarkFailed("b", "")
	// 手动把 a 的失败时间推到过期
	ff.mu.Lock()
	ff.entries["a"] = time.Now().Add(-2 * time.Hour)
	ff.mu.Unlock()

	ff.Cleanup()
	if ff.IsBlacklisted("a", "") {
		t.Error("expired entry a should be cleaned")
	}
	if !ff.IsBlacklisted("b", "") {
		t.Error("unexpired entry b should remain blacklisted")
	}
}

// TestFastFail_KeepCooldown429 验证：429 限流时不刷新冷却时间戳（保留原值，
// 冷却自然到期后由真实流量验证恢复），避免探测/限流相互顺延形成永续黑名单。
func TestFastFail_KeepCooldown429(t *testing.T) {
	ff := NewFastFailCache(time.Hour)
	ff.MarkFailed("up", "deepseek-v4-flash")
	fTime := ff.entries[ffKey("up", "deepseek-v4-flash")]

	// 429 语义：保留原冷却时间戳（MarkKeepCooldown 不写 entries）
	ff.MarkKeepCooldown("up", "deepseek-v4-flash", "status=429")
	after := ff.entries[ffKey("up", "deepseek-v4-flash")]
	if !after.Equal(fTime) {
		t.Errorf("429 should keep cooldown timestamp: %v -> %v", fTime, after)
	}
	if !ff.IsBlacklisted("up", "deepseek-v4-flash") {
		t.Error("still in cooldown, should stay blacklisted")
	}
}

// TestFastFail_NoBlacklistWhenEmpty 验证：空缓存无黑名单（等价于未启用探测时的安全路径）。
func TestFastFail_NoBlacklistWhenEmpty(t *testing.T) {
	ff := NewFastFailCache(time.Hour)
	if ff.IsBlacklisted("up", "deepseek-v4-flash") {
		t.Error("empty cache should not blacklist anything")
	}
	if ff.Names() != nil && len(ff.Names()) != 0 {
		t.Error("empty cache should have no names")
	}
}

// TestFastFailNames 验证 Names 只返回冷却期内的上游。
func TestFastFailNames(t *testing.T) {
	ff := NewFastFailCache(time.Hour)
	ff.MarkFailed("a", "")
	ff.MarkFailed("b", "")
	// 手动把 a 的失败时间推到过期
	ff.mu.Lock()
	ff.entries["a"] = time.Now().Add(-2 * time.Hour)
	ff.mu.Unlock()

	names := ff.Names()
	if len(names) != 1 || names[0].Upstream != "b" {
		t.Errorf("Names() = %v, want [b] (expired entries excluded)", names)
	}
}

// TestFastFailModelLevel 验证：模型级黑名单不影响同渠道其他模型。
func TestFastFailModelLevel(t *testing.T) {
	ff := NewFastFailCache(time.Hour)
	ff.MarkFailed("商汤", "deepseek-v4-flash")

	// deepseek-v4-flash 被拉黑
	if !ff.IsBlacklisted("商汤", "deepseek-v4-flash") {
		t.Error("deepseek-v4-flash should be blacklisted")
	}
	// 同渠道其他模型不受影响
	if ff.IsBlacklisted("商汤", "sensenova-u1-fast") {
		t.Error("sensenova-u1-fast should NOT be blacklisted")
	}
	// 渠道级判断（不区分模型）也不应误报
	if ff.IsChannelBlacklisted("商汤") {
		t.Error("channel should not be blacklisted when only model-level entry exists")
	}
}

// TestFastFailChannelFallback 验证：渠道级黑名单对所有模型生效（回退判断）。
func TestFastFailChannelFallback(t *testing.T) {
	ff := NewFastFailCache(time.Hour)
	ff.MarkFailed("商汤", "") // 渠道级

	if !ff.IsBlacklisted("商汤", "deepseek-v4-flash") {
		t.Error("model-level check should fall back to channel blacklist")
	}
	if !ff.IsBlacklisted("商汤", "sensenova-u1-fast") {
		t.Error("model-level check should fall back to channel blacklist (2)")
	}
}
