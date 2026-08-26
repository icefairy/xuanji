package proxy

import (
	"bytes"

	"github.com/tidwall/sjson"
)

// promptCacheKeyField 是 DeepSeek 官方 API 的私有扩展字段：调用方传入自定义
// 前缀缓存 key（手动管理 prompt cache）。OpenAI 协议标准中不存在此字段。
// 本网关所有上游均为 OpenAI 兼容端点（硅基流动、tokenrhythm.studio 中转、
// 商汤、mimo 等聚合站），无一识别该字段——收到即 400 UNKNOWN_FIELD。
// 客户端（pi / 各类 agent 框架）常按 DeepSeek 官方文档自动携带此字段，
// 导致网关透传后上游 400。转发前统一剥离：剥离后上游仍走自动前缀缓存，
// 功能无损失，兼容性最大化。
const promptCacheKeyField = "prompt_cache_key"

// stripPromptCacheKey 从请求体删除 prompt_cache_key 字段（若存在）。
// 返回新 body 与是否发生变更；body 不含该字段时原样返回 false（不阻断转发）。
// 放在请求体归一化链中、applyRequestOverride 之前执行：若上游 request_override
// 显式配置了该字段，仍以 override 为准重新写回（保留定向支持的余地）。
func stripPromptCacheKey(body []byte) ([]byte, bool) {
	nb, err := sjson.DeleteBytes(body, promptCacheKeyField)
	if err != nil {
		return body, false
	}
	return nb, !bytes.Equal(nb, body)
}