package config

import (
	"strconv"
	"strings"
)

// 成功率自适应权重的配置键前缀。存储用 config 表的 routing.adaptive_* 键
// （与 routing.* 同族），不新增单独的表结构。
const (
	keyAdaptivePriority       = "routing.adaptive_priority"
	keyAdaptiveWindowHours    = "routing.adaptive_window_hours"
	keyAdaptiveThreshold      = "routing.adaptive_threshold"
	keyAdaptiveRecover        = "routing.adaptive_recover"
	keyAdaptiveMinSamples     = "routing.adaptive_min_samples"
	keyAdaptiveRefreshSeconds = "routing.adaptive_refresh_seconds"
)

// applyAdaptiveDefaults 先铺默认值，再让 DB 里显式配置的键覆盖。
//
// 注意：AdaptivePriority 是本项目少数「默认 true」的 bool 配置（需求书明确要求），
// 不能照抄其它 proxy.* 开关「缺省 false」的写法；值不是 true/1 一律视为关闭，
// 保证「显式 false」能真正关掉功能（关闭时行为与未引入该功能完全一致）。
func applyAdaptiveDefaults(dst *Proxy, all map[string]string) {
	dst.AdaptiveWindowHours = DefaultAdaptiveWindowHours
	dst.AdaptiveThreshold = DefaultAdaptiveThreshold
	dst.AdaptiveRecover = DefaultAdaptiveRecover
	dst.AdaptiveMinSamples = DefaultAdaptiveMinSamples
	dst.AdaptiveRefreshSeconds = DefaultAdaptiveRefreshSeconds

	// 开关默认 true：只有显式存了非 true/1 的值才关闭
	dst.AdaptivePriority = true
	if v, ok := all[keyAdaptivePriority]; ok {
		dst.AdaptivePriority = isTruthy(strings.TrimSpace(v))
	}
	// window_hours = 0 是合法值（表示「当天」），因此这里只接受 >= 0 的解析结果，
	// 解析失败保留默认（配置项手误不应让网关拒绝启动）。
	if v, ok := all[keyAdaptiveWindowHours]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			dst.AdaptiveWindowHours = n
		}
	}
	if v, ok := all[keyAdaptiveThreshold]; ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 && f <= 1 {
			dst.AdaptiveThreshold = f
		}
	}
	if v, ok := all[keyAdaptiveRecover]; ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 && f <= 1 {
			// 恢复阈必须不低于降权阈，否则迟滞带反转，会在阈值附近来回抖动。
			// 配置失误时夹到降权阈之上（而非报错），保持「宁可不调权也不抖动」。
			if f < dst.AdaptiveThreshold {
				f = dst.AdaptiveThreshold
			}
			dst.AdaptiveRecover = f
		}
	}
	if v, ok := all[keyAdaptiveMinSamples]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			dst.AdaptiveMinSamples = n
		}
	}
	if v, ok := all[keyAdaptiveRefreshSeconds]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			dst.AdaptiveRefreshSeconds = n
		}
	}
}

// isTruthy 判定配置值是否为「开启」。与其它 bool 配置口径一致（true/1），
// 额外接受 yes/on，便于手写配置。
func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}
