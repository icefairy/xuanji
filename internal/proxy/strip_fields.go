package proxy

import (
	"bytes"
	"strings"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// stripUpstreamFields 按上游配置从请求体剥离指定顶层字段（若存在）。
// 背景：部分上游（基元律动等聚合站）对请求体里的未知字段严格校验，收到即 400 UNKNOWN_FIELD。
// 注意：prompt_cache_key / prompt_cache_retention 属缓存类私有参数，已对所有上游
// 全局剥离（见 prompt_cache.go），本机制留给你字段的定向适配。
// 按上游配置剥离：只有认不全字段的上游剥离，标准上游（opencode 等）原样透传。
//
// 返回新 body 与是否发生变更；body 不含任何配置字段时原样返回 false（不阻断转发）。
// 放在请求体归一化链中、applyRequestOverride 之前：若上游 request_override
// 显式配置了该字段，仍以 override 为准重新写回（保留定向支持的余地）。
func stripUpstreamFields(body []byte, up *config.Upstream) ([]byte, bool) {
	if up == nil || len(up.StripFields) == 0 || len(body) == 0 || !gjson.ValidBytes(body) {
		return body, false
	}
	changed := false
	nb := body
	for _, f := range up.StripFields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if !gjson.GetBytes(nb, f).Exists() {
			continue
		}
		deleted, err := sjson.DeleteBytes(nb, f)
		if err != nil {
			continue // 单字段失败不阻断整体转发
		}
		if !bytes.Equal(deleted, nb) {
			nb = deleted
			changed = true
		}
	}
	return nb, changed
}
