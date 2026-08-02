package proxy

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestFastFailProbe_Recover 验证：被标记失败的上游，后台探测返回 2xx 后解除黑名单。
func TestFastFailProbe_Recover(t *testing.T) {
	upstream, h := newTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"hi"}}]}`)
	})
	defer upstream.Close()

	h.fastFail = NewFastFailCache(time.Hour)
	h.fastFail.MarkFailed("up", "deepseek-v4-flash")
	h.probeFastFailOnce()

	if h.fastFail.IsBlacklisted("up", "deepseek-v4-flash") {
		t.Error("upstream should be recovered after successful probe")
	}
}

// TestFastFailProbe_StillDown 验证：探测仍失败（429）时保持黑名单并顺延冷却。
func TestFastFailProbe_StillDown(t *testing.T) {
	upstream, h := newTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	defer upstream.Close()

	h.fastFail = NewFastFailCache(time.Hour)
	h.fastFail.MarkFailed("up", "deepseek-v4-flash")
	h.probeFastFailOnce()

	if !h.fastFail.IsBlacklisted("up", "deepseek-v4-flash") {
		t.Error("upstream should stay blacklisted after failed probe")
	}
}

// TestFastFailProbe_NoProbeWhenNil 验证：未启用 FastFail 时探测安全跳过。
func TestFastFailProbe_NoProbeWhenNil(t *testing.T) {
	_, h := newTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {})
	h.fastFail = nil
	h.probeFastFailOnce() // 不应 panic
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
