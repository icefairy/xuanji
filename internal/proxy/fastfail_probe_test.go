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
