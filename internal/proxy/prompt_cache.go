package proxy

import (
	"bytes"

	"github.com/tidwall/sjson"
)

// promptCacheStripFields 是转发前对所有上游无条件剥离的请求体顶层字段：
//   - prompt_cache_key：DeepSeek 官方私有扩展字段（调用方自定义前缀缓存 key），
//     OpenAI 协议标准中不存在；本网关所有上游均为 OpenAI 兼容端点（聚合站/中转/
//     免费渠道），无一识别——收到即 400 UNKNOWN_FIELD。
//   - prompt_cache_retention：OpenAI 较新的缓存保留窗口参数（延长到 24h 等），
//     仅 OpenAI 官方原生端点受益；聚合类上游同样普遍不认。
//   - safety_identifier：OpenAI 较新的安全遥测参数（标识终端用户，最长 64 字符，
//     取代已废弃的 user 字段）。新版 agent 框架（Factory Droid 等）对 GPT 模型
//     每请求自动注入；仅 OpenAI 官方原生端点在滥用治理上受益，聚合类上游不认。
//
// 三者的共同特征：客户端（pi / 各类 agent 框架）自动携带、仅影响缓存策略或
// 遥测归属，不影响内容正确性，剥离后上游仍走自动前缀缓存，功能无损失。
// 全局统一剥离而非按上游配置（2026-08-27 决策，safety_identifier 随检索到的
// 同类实战问题并入）：严格校验上游漏配就 400，而真支持它的上游也只剩个小
// 优化可丢，收益远大于代价。
var promptCacheStripFields = []string{"prompt_cache_key", "prompt_cache_retention", "safety_identifier"}

// stripPromptCacheParams 从请求体删除 prompt 缓存类私有参数（若存在）。
// 返回新 body 与是否发生变更；body 不含任一字段时原样返回 false（不阻断转发）。
// 放在请求体归一化链中、applyRequestOverride 之前执行：若某上游 request_override
// 显式配置了这些字段，仍以 override 为准重新写回（保留定向支持的余地）。
func stripPromptCacheParams(body []byte) ([]byte, bool) {
	nb := body
	changed := false
	for _, f := range promptCacheStripFields {
		d, err := sjson.DeleteBytes(nb, f)
		if err != nil || bytes.Equal(d, nb) {
			continue // 单字段失败/不存在不阻断整体转发
		}
		nb = d
		changed = true
	}
	return nb, changed
}
