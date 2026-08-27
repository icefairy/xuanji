package proxy

import (
	"strings"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/tidwall/gjson"
)

// arrearKeywords 是欠费判定关键词（大小写不敏感）：上游错误响应体命中任一关键词，
// 说明该上游 API key 余额不足/配额耗尽——这类错误不会自愈（区别于 429 限流），
// 重试与自动恢复都无意义，必须人工充值后才能解除。
// 措辞刻意避开 "rate limit" / "tpm" 等纯限流词，防止把临时限流误判为欠费。
var arrearKeywords = []string{
	"余额不足",
	"欠费",
	"套餐用完了",
	"insufficient balance",         // opencode 等
	"insufficient_quota",           // OpenAI 官方余额耗尽错误码
	"quota exceeded",               // deepseek「Allocated quota exceeded」等
	"increase your quota",          // OpenAI/上游提示提升配额
	"billing hard limit",           // OpenAI hard limit reached
}

// isArrearResponse 判断上游错误响应体是否命中欠费类关键词。
// 响应体非 JSON 时退化为原文包含匹配；空响应体不判。
func isArrearResponse(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	lower := strings.ToLower(string(body))
	for _, kw := range arrearKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// SetArrearsMarker 注入欠费标记回调（nil 安全）。回调由 main 注入：
// 写 DB arrears=1 并热重载配置，使路由硬排除与健康检查停止立即生效。
func (h *Handler) SetArrearsMarker(fn func(name string)) { h.arrearsMarker = fn }

// markArrear 上游响应被判定为欠费时调用：记日志 + 触发标记回调。
// 同一上游重复触发无副作用（回调内部幂等：已欠费则不重复写库/reload）。
func (h *Handler) markArrear(up *config.Upstream, respBody []byte) {
	h.log.Warn("upstream arrear detected, marking",
		"upstream", up.Name,
		"resp_body", truncateLogLine(gjson.GetBytes(respBody, "error.message").String(), 300))
	if h.arrearsMarker == nil {
		return
	}
	h.arrearsMarker(up.Name)
}
