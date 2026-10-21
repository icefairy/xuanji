package config

import (
	"strconv"
	"strings"
)

// 时间线注入的默认渲染参数。
const (
	// DefaultTimeContextTZ 是默认时区。
	DefaultTimeContextTZ = "Asia/Shanghai"
	// DefaultTimeContextFormat 是默认时间格式（strftime 风格）。
	DefaultTimeContextFormat = "%Y-%m-%d %H:%M"
)

// loadTimeContextConfig 从 config 表（key→value）填充时间线注入配置。
//
// 设计原则：所有键都可缺省，缺省即默认值；解析失败保留默认而不是报错——
// 系统设置页的一次手误不应该让网关拒绝启动（与 retry.* 等既有键的处理一致）。
func loadTimeContextConfig(dst *TimeContextConfig, all map[string]string) {
	// 默认值：先铺一遍，再让 DB 里显式配置的键覆盖
	dst.TZ = DefaultTimeContextTZ
	dst.Format = DefaultTimeContextFormat
	dst.MinGapMinutes = 5
	dst.MaxSessions = 100000
	dst.TTLDays = 30
	dst.Persist = true
	dst.SweepMinutes = 60
	dst.FlushSeconds = 10
	dst.MaxPerKey = 1000

	boolVal := func(key string) (bool, bool) {
		v, ok := all[key]
		if !ok {
			return false, false
		}
		s := strings.TrimSpace(strings.ToLower(v))
		return s == "true" || s == "1" || s == "yes" || s == "on", true
	}
	intVal := func(key string) (int, bool) {
		v, ok := all[key]
		if !ok {
			return 0, false
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, false
		}
		return n, true
	}
	strVal := func(key string) (string, bool) {
		v, ok := all[key]
		if !ok {
			return "", false
		}
		return strings.TrimSpace(v), true
	}

	if b, ok := boolVal("proxy.time_context_enabled"); ok {
		dst.Enabled = b
	}
	if s, ok := strVal("proxy.time_context_tz"); ok && s != "" {
		dst.TZ = s
	}
	if s, ok := strVal("proxy.time_context_format"); ok && s != "" {
		dst.Format = s
	}
	if s, ok := strVal("proxy.time_context_template"); ok {
		dst.Template = s
	}
	if n, ok := intVal("proxy.time_context_min_gap"); ok && n >= 0 {
		dst.MinGapMinutes = n
	}
	if n, ok := intVal("proxy.time_context_max_sessions"); ok && n > 0 {
		dst.MaxSessions = n
	}
	if n, ok := intVal("proxy.time_context_ttl_days"); ok && n > 0 {
		dst.TTLDays = n
	}
	if b, ok := boolVal("proxy.time_context_persist"); ok {
		dst.Persist = b
	}
	// 键名是 include_gap（正面描述），内部字段是 HideGap（零值为\"显示\"）
	if b, ok := boolVal("proxy.time_context_include_gap"); ok {
		dst.HideGap = !b
	}
	if n, ok := intVal("proxy.time_context_sweep_minutes"); ok && n > 0 {
		dst.SweepMinutes = n
	}
	if n, ok := intVal("proxy.time_context_flush_seconds"); ok && n > 0 {
		dst.FlushSeconds = n
	}
	if n, ok := intVal("proxy.time_context_max_per_key"); ok && n > 0 {
		dst.MaxPerKey = n
	}
}
