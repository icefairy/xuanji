package proxy

import (
	"time"
)

// StartFastFailProbe 启动后台冷却维护任务（懒加载模式）。
//
// 注意：**不再主动探测上游**。历史实现会对黑名单条目主动发最小 chat 请求验证恢复，
// 但探测请求与真实流量语义不一致会导致误判：部分严格校验上游（如 b.ai 要求
// max_tokens>2，探测却用 max_tokens=1）会对探测请求返回 400，触发 MarkFailed
// 顺延冷却 → 探测永远失败 → 上游被永久拉黑（UI 持续红色），形成"探测死亡螺旋"。
//
// 懒加载语义（由真实流量驱动）：
//   - 请求失败 → 真实流量路径 MarkFailed 标记冷却（completions.go/proxy.go）
//   - 请求成功 → 真实流量路径 MarkSuccess 解除黑名单
//   - 冷却到期 → 本任务 Cleanup 自动清理，无请求到达时默认视为可用（放行）
//   - 上游真实故障时：真实请求失败 → 自动切换下一上游重试（primary_backup 策略），
//     并在冷却期内不再尝试该故障上游
//
// 因此本任务只做冷却到期清理，不发送任何探测请求（零 token 消耗、不会误触上游限流）。
func (h *Handler) StartFastFailProbe(interval time.Duration) (stop func()) {
	stopCh := make(chan struct{})
	ticker := time.NewTicker(interval)
	go func() {
		for {
			select {
			case <-ticker.C:
				// 懒加载：仅清理冷却到期的黑名单条目（到期即放行，不探测验证）
				h.fastFail.Cleanup()
			case <-stopCh:
				ticker.Stop()
				return
			}
		}
	}()
	// 首次启动立即清理一次，不等间隔
	h.fastFail.Cleanup()
	return func() { close(stopCh) }
}