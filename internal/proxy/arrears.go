package proxy

import (
	"strings"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/store"
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
	"insufficient balance", // opencode 等
	"insufficient_quota",   // OpenAI 官方余额耗尽错误码
	"quota exceeded",       // deepseek「Allocated quota exceeded」等
	"increase your quota",  // OpenAI/上游提示提升配额
	"billing hard limit",   // OpenAI hard limit reached
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

// SetArrearsMarker 注入上游级欠费标记回调（nil 安全）。回调由 main 注入：
// 写 DB upstreams.arrears=1 并热重载配置，使路由硬排除与健康检查停止立即生效。
func (h *Handler) SetArrearsMarker(fn func(name string)) { h.arrearsMarker = fn }

// SetArrearsModelMarker 注入模型级欠费标记回调（per_model_billing 上游用）。
// 回调签名 (upstream, clientModel, reason)：写 DB upstream_model_arrears 表。
func (h *Handler) SetArrearsModelMarker(fn func(upstream, model, reason string)) {
	h.arrearsModelMarker = fn
}

// LoadModelArrears 从 DB 加载模型级欠费记录进内存缓存（启动/reload 后调用）。
func (h *Handler) LoadModelArrears(rows []store.ModelArrearRow) {
	h.modelArrearsMu.Lock()
	defer h.modelArrearsMu.Unlock()
	h.modelArrears = make(map[string]bool, len(rows))
	for _, r := range rows {
		h.modelArrears[modelArrearKey(r.Upstream, r.Model)] = true
	}
}

// modelArrearKey 构造模型级欠费缓存 key（与 fastfail 的 ffKey 同风格）。
func modelArrearKey(upstream, model string) string {
	return upstream + "::" + model
}

// IsModelArrear 查询指定上游的指定客户端模型是否处于欠费标记中。
func (h *Handler) IsModelArrear(upstream, model string) bool {
	h.modelArrearsMu.RLock()
	defer h.modelArrearsMu.RUnlock()
	return h.modelArrears != nil && h.modelArrears[modelArrearKey(upstream, model)]
}

// markModelArrear 写内存缓存并触发落库回调（幂等：已标记则跳过，防并发重复写库）。
func (h *Handler) markModelArrear(upstream, model, reason string) {
	key := modelArrearKey(upstream, model)
	h.modelArrearsMu.Lock()
	already := h.modelArrears != nil && h.modelArrears[key]
	if !already {
		if h.modelArrears == nil {
			h.modelArrears = make(map[string]bool)
		}
		h.modelArrears[key] = true
	}
	h.modelArrearsMu.Unlock()
	if already || h.arrearsModelMarker == nil {
		return
	}
	h.arrearsModelMarker(upstream, model, reason)
}

// ClearModelArrearCache 清除模型级欠费内存缓存（admin 测试上游成功时调用）。
// DB 记录由调用方直接删除，此处仅同步内存；reload 后 LoadModelArrears 以 DB 为准重建。
func (h *Handler) ClearModelArrearCache(upstream, model string) {
	h.clearModelArrear(upstream, model)
}

// clearModelArrear 清除内存缓存中的模型级欠费标记（落库由调用方直接操作 DB，
// reload 后 LoadModelArrears 会以 DB 为准重建缓存）。
func (h *Handler) clearModelArrear(upstream, model string) {
	h.modelArrearsMu.Lock()
	delete(h.modelArrears, modelArrearKey(upstream, model))
	h.modelArrearsMu.Unlock()
}

// markArrear 上游响应被判定为欠费时调用：按上游配置决定粒度并记日志+触发回调。
//   - up.PerModelBilling=false（默认）：上游级——整条上游停路由停健康检查；
//     model 参数用于日志展示。适合单主力模型或全渠道共享额度的上游。
//   - up.PerModelBilling=true：模型级——仅跳过该上游上该模型，其它模型继续可用；
//     适合阿里云百炼等每模型独立免费额度的平台。
//
// 同一粒度重复触发无副作用（回调内部幂等）。
func (h *Handler) markArrear(up *config.Upstream, model string, respBody []byte) {
	reason := truncateLogLine(gjson.GetBytes(respBody, "error.message").String(), 300)
	if up.PerModelBilling {
		h.log.Warn("upstream model arrear detected, marking",
			"upstream", up.Name,
			"model", model,
			"resp_body", reason)
		h.markModelArrear(up.Name, model, reason)
		return
	}
	h.log.Warn("upstream arrear detected, marking",
		"upstream", up.Name,
		"model", model,
		"resp_body", reason)
	if h.arrearsMarker == nil {
		return
	}
	h.arrearsMarker(up.Name)
}
