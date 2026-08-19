// Package proxy 实现 OpenAI chat/completions 协议的转发与 SSE 透传。
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/health"
	"github.com/icefairy/xuanji/internal/router"
	"github.com/icefairy/xuanji/internal/store"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// upstreamTimeout 是上游连接与（非流式）整体请求的超时时间。
const upstreamTimeout = 60 * time.Second

// upstreamTimeoutFor 返回可配置的上游超时（retry.upstream_timeout 秒），未配置时回退 60s。
func upstreamTimeoutFor(cfg *config.Config) time.Duration {
	if cfg != nil && cfg.Retry.UpstreamTimeout > 0 {
		return time.Duration(cfg.Retry.UpstreamTimeout) * time.Second
	}
	return upstreamTimeout
}

// chatPath 拼接在 upstream.BaseURL 之后，构成 chat/completions 转发端点。
// 用 /v1/ 前缀兼容 OpenAI 标准；部分上游（如硅基流动）双兼容，部分（如基元律动、商汤）只认 /v1/。
const chatPath = "/v1/chat/completions"

// Handler 处理 POST /v1/chat/completions 的转发逻辑。
type Handler struct {
	cfg       *config.Config
	router    *router.Router
	health    *health.Checker
	client    *http.Client
	log       *slog.Logger
	recorder  *store.Recorder                                         // 指标记录器；nil 时跳过记录
	fastFail  *FastFailCache                                          // 快速失败缓存；nil 时不启用
	tokenizer *Tokenizer                                              // token 计数器；nil 时跳过估算回退
	discounts []store.Discount                                        // 渠道优惠时段，用于同 tier 同 weight 内折扣优先排序
	keyName   func(r *http.Request) string                            // 下游 API Key 展示名（统计用）；nil 时记录空
	priceFor  func(model string) (input, cache, out float64, ok bool) // 模型单价查询；nil 时不计费
	cooldowns sync.Map                                                // 上游冷却表：map[string]time.Time, key="upstream:model"，value=冷却到期时间
	reasoning *ReasoningCache                                         // reasoning_content 缓存（key=tool_call_id，thinking 模式回传用）；nil 时不启用
}

// New 创建转发 Handler，共享一个 60s 连接超时的 HTTP 客户端。
// health 用于健康过滤与失败切换；为 nil 时退化为只转发路由列表第一个上游。
func New(cfg *config.Config, rt *router.Router, hc *health.Checker) *Handler {
	transport := &http.Transport{
		DialContext: (&net.Dialer{Timeout: upstreamTimeoutFor(cfg)}).DialContext,
	}
	return &Handler{
		cfg:       cfg,
		router:    rt,
		health:    hc,
		client:    &http.Client{Transport: transport},
		log:       slog.Default(),
		tokenizer: NewTokenizer(),
		reasoning: NewReasoningCache(0),
	}
}

// SetRecorder 注入指标记录器（nil 安全；测试不需要调用）。
func (h *Handler) SetRecorder(r *store.Recorder) {
	h.recorder = r
}

// SetKeyName 注入下游 API Key 展示名解析器（统计按 Key 维度用；nil 时记录空）。
func (h *Handler) SetKeyName(fn func(r *http.Request) string) {
	h.keyName = fn
}

// recordAPIKey 解析当前请求的下游 API Key 展示名（未注入解析器时返回空）。
func (h *Handler) recordAPIKey(r *http.Request) string {
	if h.keyName == nil {
		return ""
	}
	return h.keyName(r)
}

// SetTokenizer 注入 token 计数器（nil 安全；默认 NewTokenizer()）。
func (h *Handler) SetTokenizer(tz *Tokenizer) {
	h.tokenizer = tz
}

// SetFastFail 设置快速失败缓存（nil 时不启用）。
func (h *Handler) SetFastFail(f *FastFailCache) { h.fastFail = f }

// SetPriceFor 注入模型单价查询函数（nil 时不计费）。
// 参数为上游真实模型名（计费口径：按上游真实模型名定价）。
func (h *Handler) SetPriceFor(fn func(model string) (input, cache, out float64, ok bool)) {
	h.priceFor = fn
}

// cacheReasoningEnabled 判断 reasoning_content 缓存功能是否启用：
// 开关 proxy.cache_reasoning_content 默认开启（cfg 为 nil 或未配置时按开启处理），
// 且缓存对象已初始化。关闭时缓存不写、注入不执行，body 原样透传。
func (h *Handler) cacheReasoningEnabled() bool {
	if h.reasoning == nil {
		return false
	}
	return h.cfg == nil || h.cfg.Proxy.CacheReasoningContent
}

// calcCost 按上游真实模型名 + usage 计算本次请求费用（元）。
// 计费口径：
//   - 输入 token 分缓存命中/未命中两档价
//   - 缓存命中只影响输入（prompt_cache_hit_tokens）
//   - 输出单独一档价
//
// 未配置价格表（或模型无默认价）时返回 0。
func (h *Handler) calcCost(upstreamModel, clientModel string, promptTokens, completionTokens, cacheHit, cacheMiss int64) float64 {
	if h.priceFor == nil {
		return 0
	}
	// 优先按上游真实模型名查价，其次按客户端模型名，最后默认价（store.PriceFor 内部兜底 '*'）
	input, cache, out, ok := h.priceFor(upstreamModel)
	if !ok {
		input, cache, out, ok = h.priceFor(clientModel)
	}
	if !ok || (input <= 0 && cache <= 0 && out <= 0) {
		return 0
	}
	// 无缓存统计（hit=miss=0 且输入>0）时，输入按未命中价全额计费：
	// 上游没报缓存命中，保守按全价（未命中价）算，避免输入 token 白嫖。
	if cacheHit <= 0 && cacheMiss <= 0 && promptTokens > 0 {
		cacheMiss = promptTokens
	}
	// token 单价：元/百万token → 每 token 价格
	const perMillion = 1e6
	cost := float64(cacheMiss)/perMillion*input +
		float64(cacheHit)/perMillion*cache +
		float64(completionTokens)/perMillion*out
	return cost
}

// SetDiscounts 注入渠道优惠时段列表（用于同 tier 同 weight 内折扣优先排序）。
func (h *Handler) SetDiscounts(d []store.Discount) { h.discounts = d }

// isDiscountActive 检查指定上游+模型当前时间是否处于优惠时段。
func (h *Handler) isDiscountActive(upstream, model string) bool {
	if h.discounts == nil {
		return false
	}
	now := time.Now()
	nowMin := uint16(now.Hour())*60 + uint16(now.Minute())
outer:
	for _, d := range h.discounts {
		if d.Upstream != "" && d.Upstream != upstream {
			continue
		}
		if d.ModelPattern != "" && d.ModelPattern != "*" {
			parts := strings.Split(d.ModelPattern, ",")
			matched := false
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if p == "*" || p == model {
					matched = true
					break
				}
				if strings.HasPrefix(p, "*") && strings.HasSuffix(p, "*") {
					mid := strings.Trim(p, "*")
					if strings.Contains(model, mid) {
						matched = true
						break
					}
				}
			}
			if !matched {
				continue outer
			}
		}
		startH, startM, endH, endM := 0, 0, 0, 0
		if _, err := fmt.Sscanf(d.StartTime, "%d:%d", &startH, &startM); err != nil {
			continue
		}
		if _, err := fmt.Sscanf(d.EndTime, "%d:%d", &endH, &endM); err != nil {
			continue
		}
		startMin := uint16(startH)*60 + uint16(startM)
		endMin := uint16(endH)*60 + uint16(endM)
		if startMin == endMin {
			return true // 全天
		}
		if startMin < endMin {
			if nowMin >= startMin && nowMin <= endMin {
				return true
			}
		} else {
			// 跨天：23:00~07:00
			if nowMin >= startMin || nowMin <= endMin {
				return true
			}
		}
	}
	return false
}

// IsEmptyCompletion 判定响应是否为"空内容完成"：思考型模型 max_tokens 不足且
// **连思考都没有**（choices[0] 存在、content 空、无 tool_calls、无 reasoning/reasoning_content、finish_reason=length）。
// 返回 true 表示该响应不可用应切换候选；tool_calls / 正常内容 / 空 choices 不误伤。
// ⚠ 思考字段有值 = 模型正常在思考，只是 max_tokens 短未输出正文，这是正常响应（客户端能看到思考内容），
//
//	不判空、不切换候选。思考字段名因推理引擎而异，必须同时检查两种：
//	- `reasoning_content`：DeepSeek R1/V4、Kimi、GLM、MiniMax、vLLM(Qwen3 Thinking) 等主流 OpenAI 兼容格式
//	- `reasoning`：商汤日日新等部分厂商（2026-08 实测确认）
func IsEmptyCompletion(data []byte) bool {
	choices := gjson.GetBytes(data, "choices")
	if !choices.Exists() || !choices.IsArray() || len(choices.Array()) == 0 {
		return false
	}
	c0 := choices.Array()[0]
	msg := c0.Get("message")
	if !msg.Exists() {
		return false
	}
	content := msg.Get("content")
	if content.Exists() && content.Type != gjson.Null && content.String() != "" {
		return false // 有内容，即使 length 截断也不算空
	}
	if msg.Get("tool_calls").Exists() {
		return false
	}
	// 思考字段有值 → 模型在思考中，正常响应（商汤 reasoning / DeepSeek reasoning_content）
	if msg.Get("reasoning").String() != "" || msg.Get("reasoning_content").String() != "" {
		return false
	}
	return c0.Get("finish_reason").String() == "length"
}

// apiError 是 OpenAI 标准错误响应体。
type apiError struct {
	Error apiErrorDetail `json:"error"`
}

// apiErrorDetail 是 OpenAI 错误对象的字段。
type apiErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

// ChatCompletions 处理 POST /v1/chat/completions。
func (h *Handler) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w}
	start := time.Now()

	var model, upstream string
	var stream bool

	defer func() {
		h.log.Info("chat/completions",
			"method", r.Method,
			"path", r.URL.Path,
			"model", model,
			"stream", stream,
			"upstream", upstream,
			"status", rec.status,
			"duration", time.Since(start).String(),
		)
	}()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(rec, http.StatusBadRequest, "failed to read request body", "invalid_request_error", "invalid_request")
		return
	}
	model = gjson.GetBytes(body, "model").String()
	stream = gjson.GetBytes(body, "stream").Bool()
	if model == "" {
		writeError(rec, http.StatusBadRequest, "model is required", "invalid_request_error", "missing_model")
		return
	}

	// 多条 system 消息合并为一条放到最前（部分上游不允许多 system，会 400）
	if nb, changed := MergeSystemMessages(body); changed {
		body = nb
	}

	upstreams, strategy, err := h.router.Route(model)
	if err != nil {
		writeError(rec, http.StatusNotFound, fmt.Sprintf("no upstream found for model %q", model), "invalid_request_error", "model_not_found")
		return
	}

	// 多模态兑底：请求带图像且当前命中的规则不支持多模态（vision=0）且配置了
	// 兑底聚合模型（vision_fallback 非空）时，把 model 改写为兑底模型重新路由转发。
	// 兑底模型名是聚合模型名（如 "flash"），由对应上游的 model_mapping 映射到真实模型名。
	// vision=0 且无兑底、或 vision=1 的规则行为与原来完全一致（不触发兑底）。
	if isMultimodalRequest(body) {
		if rule := h.router.FindRule(model); rule != nil && !rule.Vision && strings.TrimSpace(rule.VisionFallback) != "" {
			oldModel := model
			model = rule.VisionFallback
			h.log.Info("vision fallback", "from", oldModel, "to", model, "reason", "multimodal request")
			upstreams, strategy, err = h.router.Route(model)
			if err != nil {
				writeError(rec, http.StatusNotFound, fmt.Sprintf("no upstream found for vision fallback model %q", model), "invalid_request_error", "model_not_found")
				return
			}
		}
	}

	candidates := h.selectCandidates(upstreams, strategy, model)
	retryCount := 0
	maxRetries := h.cfg.Retry.MaxRetries
	connIssues := false // 是否有连接类错误（网络断/超时），用于判断全局网络问题
	for i := 0; i < len(candidates) && retryCount <= maxRetries; i++ {
		// 客户端已断开（context canceled）：不再尝试任何上游，也不标记 fastfail——
		// 否则一次断连会把所有候选全拉黑 60 分钟（2026-08-02 实测：6 个上游全被误拉黑）
		if r.Context().Err() != nil {
			h.log.Warn("client disconnected, aborting retry loop",
				"model", model, "error", r.Context().Err())
			break
		}
		up := candidates[i]
		handled, retryable, ferr, _, _, _, _ := h.forwardOnce(rec, r, body, up, model, stream, false)
		if ferr != nil && h.health != nil {
			h.health.MarkFailure(up.Name)
		}
		// 连接类错误（网络不可达/超时/连接拒绝）说明可能是本地网络问题而非上游故障
		if ferr != nil {
			var netErr net.Error
			if errors.As(ferr, &netErr) || errors.Is(ferr, context.DeadlineExceeded) {
				connIssues = true
			}
		}
		if handled {
			upstream = up.Name
			// 成功请求后对该上游标记冷却（防 429 限流），仅对 CooldownUpstreams 匹配的上游生效
			h.markCooldown(up.Name, model)
			return
		}
		h.log.Warn("upstream failed, trying next",
			"upstream", up.Name, "model", model, "error", ferr)
		if !retryable {
			break
		}
		retryCount++
		errString := ferr.Error()
		isRateLimit := strings.Contains(errString, "429")
		// 429 限流是 per-key 的：重试同一个上游（相同 API key）必然再次 429，
		// 不应重置索引反复打同一 key。跳过该上游，由下一轮 i++ 走到下一个候选
		//（不同 API key）。连接类错误等其他 retryable 错误可重置从头再试（网络可能恢复）。
		if !isRateLimit && i == len(candidates)-1 && retryCount < maxRetries {
			i = -1 // 下一轮循环 i++ 变为 0
		}
	}
	// 兜底：正常流程最后一个候选 handled=true，不应到达
	// ⚠ 全部候选失败 + 含连接类错误 = 本地网络/全局网络问题（非单个上游故障）——
	// 保留 fastfail 黑名单毫无意义（网络恢复前任何上游都连不上），
	// 反而导致网络恢复后要等 probe（35 分钟一轮）逐个解除。
	// 清空本规则涉及上游的黑名单，让网络恢复后立即可用（2026-08-02 修复）。
	// 仅 HTTP 错误（上游真实故障，如 502/429）不清空——单上游规则依赖黑名单跳过故障上游。
	if connIssues && h.fastFail != nil {
		cleared := 0
		for _, up := range candidates {
			// 网络问题清空该上游 model 相关的全部黑名单（客户端名 + 所有真实模型名）
			h.clearUpstreamBlacklist(up, model)
			cleared++
		}
		if cleared > 0 {
			h.log.Warn("all upstreams failed with connection errors (likely network issue), cleared fastfail blacklist",
				"model", model, "cleared", cleared)
		}
	}
	writeError(rec, http.StatusBadGateway, "all upstreams failed", "server_error", "upstream_unreachable")
}

// realModels 返回 model 在该上游映射后的所有真实模型名；无映射时返回 [model]。
func realModels(up *config.Upstream, model string) []string {
	if up == nil {
		return []string{model}
	}
	if mapped, ok := up.ModelMapping[model]; ok && mapped != "" {
		return strings.Split(mapped, "|")
	}
	return []string{model}
}

// upstreamSupportsModel 判断上游是否声明提供该模型：
//   - models 为空 = 未声明模型列表，视为全支持（兼容旧配置）
//   - 客户端模型名在 models 列表中 → 支持
//   - 客户端模型名在 model_mapping 中有映射 → 支持（映射可能指向其他真实模型）
//   - 客户端模型名是 models 中某项的真实模型名（如 model_mapping 的 value）→ 支持
//   - 其余情况返回 false（路由该上游必然 404/400）
// setUpstreamAuth 按上游类型设置认证请求头。
// 默认 OpenAI 惯例 Authorization: Bearer <key>；Dots API（dots.ai）要求 api-key 头。
func setUpstreamAuth(req *http.Request, up *config.Upstream) {
	if up.IsDots() {
		req.Header.Set("api-key", up.APIKey)
		req.Header.Del("Authorization")
		return
	}
	req.Header.Set("Authorization", "Bearer "+up.APIKey)
}

func upstreamSupportsModel(up *config.Upstream, model string) bool {
	if up == nil {
		return true
	}
	// 未声明模型列表：不做限制
	if len(up.Models) == 0 {
		return true
	}
	for _, m := range up.Models {
		if m == model {
			return true
		}
	}
	// 有 model_mapping 映射该模型 → 支持
	if _, ok := up.ModelMapping[model]; ok {
		return true
	}
	// model 可能是某个映射的真实模型名（如 deepseek-v4-flash 映射到 deepseek-v4-flash-0731，
	// 客户端直接发真实模型名也应放行）
	for _, m := range up.Models {
		if m == model {
			return true
		}
	}
	for _, mapped := range up.ModelMapping {
		for _, real := range strings.Split(mapped, "|") {
			if real == model {
				return true
			}
		}
	}
	return false
}

// pickAvailableModel 为该上游选择本次请求可用的真实模型名：
// 竖线展开所有候选真实模型，过滤掉处于 fastfail 黑名单的，随机选一个；
// 全部被拉黑或无候选时返回空串（调用方应跳过该上游）。
// 无 fastfail 时等价于 MapModel 的随机逻辑（保持既有行为）。
func (h *Handler) pickAvailableModel(up *config.Upstream, model string) string {
	cands := realModels(up, model)
	if h.fastFail == nil || up == nil {
		if len(cands) == 0 {
			return model
		}
		return cands[rand.Intn(len(cands))]
	}
	var avail []string
	for _, c := range cands {
		if c == "" {
			continue
		}
		if !h.fastFail.IsBlacklisted(up.Name, c) {
			avail = append(avail, c)
		}
	}
	if len(avail) == 0 {
		return ""
	}
	return avail[rand.Intn(len(avail))]
}

// clearUpstreamBlacklist 清除该上游 model 相关的全部 fastfail 黑名单
// （客户端模型名 + 所有映射后的真实模型名），用于全局网络故障恢复。
func (h *Handler) clearUpstreamBlacklist(up *config.Upstream, model string) {
	if h.fastFail == nil || up == nil {
		return
	}
	names := append([]string{model}, realModels(up, model)...)
	for _, n := range names {
		h.fastFail.MarkSuccess(up.Name, n)
	}
}

// needCooldownForUpstream 检查上游是否需要 per-key 冷却（名称匹配 CooldownUpstreams 前缀）。
func (h *Handler) needCooldownForUpstream(name string) bool {
	if len(h.cfg.Proxy.CooldownUpstreams) == 0 {
		return false
	}
	for _, prefix := range h.cfg.Proxy.CooldownUpstreams {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// markCooldown 标记上游进入冷却（成功请求后调用）。
func (h *Handler) markCooldown(name, model string) {
	if name == "" || model == "" {
		return
	}
	if !h.needCooldownForUpstream(name) {
		return
	}
	seconds := h.cfg.Proxy.CooldownSeconds
	if seconds <= 0 {
		seconds = 1
	}
	key := name + ":" + model
	h.cooldowns.Store(key, time.Now().Add(time.Duration(seconds)*time.Second))
	h.log.Debug("upstream cooldown", "upstream", name, "model", model, "seconds", seconds)
}

// isCooldown 检查上游是否在冷却期内。过期条目懒清理。
func (h *Handler) isCooldown(name, model string) bool {
	key := name + ":" + model
	v, ok := h.cooldowns.Load(key)
	if !ok {
		return false
	}
	until := v.(time.Time)
	if time.Now().After(until) {
		h.cooldowns.Delete(key)
		return false
	}
	return true
}

// selectCandidates 依据健康状态与统一优先级选出转发候选：
// 5. 同 tier 同 weight 同折扣状态内：网络延迟低的优先（未测过延迟的排最后）
// 失败切换由调用方按候选列表顺序逐个尝试：同 tier 内失败自动试下一个，
// 同 tier 全部失败自动升到上一级计费类型（免费→包月→按量）。
func (h *Handler) selectCandidates(ups []*config.Upstream, strategy, model string) []*config.Upstream {
	// 禁用过滤：跳过 enabled=false 的上游（管理页手动禁用，不参与转发）
	var enabledUps []*config.Upstream
	for _, u := range ups {
		if u.Enabled {
			enabledUps = append(enabledUps, u)
		}
	}
	if len(enabledUps) > 0 {
		ups = enabledUps
	}
	// 快速失败过滤：跳过冷却期内的上游（按渠道+模型两级粒度判断）。
	// 一对多映射：该上游所有真实模型名都在黑名单才跳过；只要有一个可用就保留。
	if h.fastFail != nil {
		var filtered []*config.Upstream
		for _, u := range ups {
			if h.pickAvailableModel(u, model) != "" {
				filtered = append(filtered, u)
			}
		}
		if len(filtered) > 0 {
			ups = filtered
		} // 全部被黑名单时不缩减，fallback 到原有逻辑
	}
	// 请求级冷却过滤：上游刚完成一次成功请求，cooldown 期内不再分配新请求。
	// 防止同一 API key 被并发打爆触发 429。冷却期很短（1～2 秒），过期后自动恢复。
	// 全部在冷却中时保留原列表（不拒绝请求，只是降级到无冷却状态）。
	var cooled []*config.Upstream
	for _, u := range ups {
		if !h.isCooldown(u.Name, model) {
			cooled = append(cooled, u)
		}
	}
	if len(cooled) > 0 {
		ups = cooled
	}
	// 模型支持过滤：上游声明了 models 列表但既不含该模型、也没有对应的 model_mapping，
	// 说明该上游根本不提供此模型，路由过去必然 404/400。直接跳过，避免浪费一次调用。
	// （models 为空 = 未声明，视为全支持，保持兼容）
	var supported []*config.Upstream
	for _, u := range ups {
		if !upstreamSupportsModel(u, model) {
			h.log.Debug("skip upstream: model not supported",
				"upstream", u.Name, "model", model, "models", u.Models)
			continue
		}
		supported = append(supported, u)
	}
	if len(supported) > 0 {
		ups = supported
	} // 全部不支持时不缩减，fallback 到原有逻辑（宁发 404 也不丢请求）
	if h.health != nil {
		healthy := h.health.HealthyUpstreams(ups)
		if len(healthy) == 0 {
			// 兜底：全都不健康时也不能乱选——按 tier 升序（subscription→free→payg），
			// 让按量付费渠道永远排最后。直接 ups[:1] 会返回规则顺序第一个（往往是 payg）。
			sorted := make([]*config.Upstream, len(ups))
			copy(sorted, ups)
			sort.SliceStable(sorted, func(i, j int) bool {
				return sorted[i].TierWeight() < sorted[j].TierWeight()
			})
			h.log.Warn("no healthy upstream for model, falling back to first (tier-sorted)",
				"model", model, "total", len(ups), "picked", sorted[0].Name, "tier", sorted[0].TierWeight())
			return sorted[:1]
		}
		ups = healthy
	}

	// 按 tier 分组：subscription(0) < free(1) < payg(2)
	grouped := make(map[int][]*config.Upstream)
	var tiers []int
	for _, u := range ups {
		tw := u.TierWeight()
		if _, ok := grouped[tw]; !ok {
			tiers = append(tiers, tw)
		}
		grouped[tw] = append(grouped[tw], u)
	}
	sort.Ints(tiers)

	var out []*config.Upstream
	for _, tw := range tiers {
		group := grouped[tw]
		// 统一优先级：同 tier 内 weight 降序；同 weight 内优惠时段优先；同权重同折扣延迟低优先（稳定排序保序）
		sort.SliceStable(group, func(i, j int) bool {
			wi, wj := group[i].Weight, group[j].Weight
			if wi != wj {
				return wi > wj
			}
			di := h.isDiscountActive(group[i].Name, model)
			dj := h.isDiscountActive(group[j].Name, model)
			if di != dj {
				return di
			}
			// 同权重同折扣：延迟低优先；Latency()==0 表示未测过，排最后
			li := h.latencyRank(group[i].Name)
			lj := h.latencyRank(group[j].Name)
			if li != lj {
				if li == 0 {
					return false
				}
				if lj == 0 {
					return true
				}
				return li < lj
			}
			return false
		})
		out = append(out, group...)
	}
	// 同 tier 同 weight 同折扣同延迟：随机打乱（所有条件相同，避免固定原序导致流量倾斜）
	for i := 0; i < len(out); {
		j := i
		for j < len(out) && out[j].TierWeight() == out[i].TierWeight() && out[j].Weight == out[i].Weight {
			// 细分：折扣状态 + 延迟完全相同才在一组
			sameDiscount := h.isDiscountActive(out[i].Name, model) == h.isDiscountActive(out[j].Name, model)
			sameLatency := h.latencyRank(out[i].Name) == h.latencyRank(out[j].Name)
			if !(sameDiscount && sameLatency) {
				break
			}
			j++
		}
		if j-i > 1 {
			rand.Shuffle(j-i, func(a, b int) {
				out[i+a], out[i+b] = out[i+b], out[i+a]
			})
		}
		i = j
	}
	return out
}

// latencyRank 返回上游最近健康检查延迟的毫秒数，用于同权重候选的延迟排序；
// 0 表示未测过延迟（health 为 nil 或尚无探测数据），排序时排最后。
func (h *Handler) latencyRank(name string) int64 {
	if h.health == nil {
		return 0
	}
	return int64(h.health.Latency(name) / time.Millisecond)
}

// forwardOnce 向单个上游转发一次 chat/completions 请求。
// handled=true 表示响应已写入客户端（或已决定不再切换）；retryable=true 表示本次
// 失败可尝试下一个候选；err 非 nil 表示该上游转发失败（供调用方反馈到健康状态）。
// promptTokens/completionTokens 返回本次请求的 token 数（上游 usage 或 tokenizer 估算）；
// promptCacheHitTokens/promptCacheMissTokens 返回上游 usage 里的前缀缓存命中/未命中 token 数。
// last 为 true 时，上游的连接错误 / 5xx / 429 会直接生成最终响应，不再返回可重试。
func (h *Handler) forwardOnce(w http.ResponseWriter, r *http.Request, body []byte, up *config.Upstream, model string, stream bool, last bool) (handled, retryable bool, err error, promptTokens, completionTokens, promptCacheHitTokens, promptCacheMissTokens int64) {
	start := time.Now()
	var status int
	// 流式转发期间客户端提前断开（streamCopy 写响应失败）：日志状态记为 499，
	// 避免"中断"被误记为 200 污染统计（客户端实际收到 200 头后断流）。
	var streamInterrupted bool
	upstreamModel := h.pickAvailableModel(up, model)
	defer func() {
		if h.recorder == nil || !handled {
			return
		}
		if sr, ok := w.(*statusRecorder); ok {
			status = sr.status
		}
		if streamInterrupted && status >= 200 && status < 400 {
			status = 499
		}
		cost := 0.0
		if status >= 200 && status < 400 && (promptTokens > 0 || completionTokens > 0) {
			cost = h.calcCost(upstreamModel, model, promptTokens, completionTokens, promptCacheHitTokens, promptCacheMissTokens)
		}
		h.recorder.Record(store.Record{
			Timestamp:             time.Now(),
			Upstream:              up.Name,
			Model:                 model,
			UpstreamModel:         upstreamModel,
			Cost:                  cost,
			Endpoint:              "chat",
			Status:                status,
			DurationMS:            time.Since(start).Milliseconds(),
			PromptTokens:          promptTokens,
			CompletionTokens:      completionTokens,
			Tokens:                promptTokens + completionTokens,
			APIKey:                h.recordAPIKey(r),
			ClientAddr:            r.RemoteAddr, // 客户端地址 "IP:port"，用于区分调用程序
			UserAgent:             r.UserAgent(), // 客户端 UA，程序识别最强信号
			PromptCacheHitTokens:  promptCacheHitTokens,
			PromptCacheMissTokens: promptCacheMissTokens,
		})
	}()
	reqBody := body
	if upstreamModel == "" {
		// 所有真实模型都在 fastfail 冷却期（selectCandidates 全被过滤时的 fallback 场景）。
		// 退化为 MapModel 随机选一个真实模型名继续尝试：连接失败 → connIssues 清黑名单，
		// 成功 → MarkSuccess 恢复。保持原 fallback 语义（黑名单只是软跳过，可被真实请求纠正）。
		upstreamModel = h.router.MapModel(up, model)
	}
	if upstreamModel != model {
		if reqBody, err = sjson.SetBytes(body, "model", upstreamModel); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to rewrite model field", "server_error", "")
			return true, false, nil, 0, 0, 0, 0
		}
	}
	// 思考深度归一化：客户端标准 reasoning_effort → 目标模型实际思考参数
	// （DeepSeek 透传/适配档位、商汤转 output_config.effort、Kimi/GLM 转 thinking.type 等）
	// 先按最佳思考等级配置注入/覆盖（auto=补推荐值，force=强制覆盖），再归一化。
	// developer 角色兼容：部分上游不认 OpenAI 新角色 developer（只认 system/user/assistant/tool），
	// 会返回 400。默认开启，把 role=developer 归一化为 role=system；关闭时原样透传。
	if h.cfg.Proxy.NormalizeDeveloperRole {
		if nb, changed := normalizeDeveloperRole(reqBody); changed {
			reqBody = nb
		}
	}
	// max_tokens 归一化：<=0 删除（上游不接受 0/负数），> 上游上限时 clamp（防 WorkBuddy 等客户端按窗口自动填超大值导致 400）
	if nb, changed := normalizeMaxTokens(reqBody, up); changed {
		reqBody = nb
	}
	if nb, changed := applyBestEffort(reqBody, model, h.cfg); changed {
		reqBody = nb
	}
	if nb, changed := normalizeThinkingEffort(reqBody, upstreamModel); changed {
		reqBody = nb
	}
	// 请求体复写：网关层强制覆盖请求参数（如关思考、固定采样参数）
	if nb, changed := applyRequestOverride(reqBody, up.RequestOverride); changed {
		reqBody = nb
	}
	// image_url 拍平：OpenAI 标准嵌套对象 {image_url:{url}} → 上游认识的平铺字符串 {image_url:url}。
		// vllm/agnes 等后端不认嵌套对象，收到报 400 Unexpected item type；Dots 例外（要求标准嵌套，跳过拍平）。
		if !up.IsDots() {
			if nb, changed := normalizeImageURLFlat(reqBody); changed {
				reqBody = nb
			}
		}
	// thinking 模式 reasoning_content 回传兼容：客户端 agent（pi / Claude Code 等）
	// 消息规范化时可能丢掉历史 assistant 消息的 reasoning_content，DeepSeek thinking
	// 模式多轮 tool-calling 要求原样回传否则上游 400。用上一轮响应缓存的
	// tool_call_id → reasoning_content 补回。尽力而为：命中才注入，未命中原样转发。
	if h.cacheReasoningEnabled() {
		if nb, changed := injectReasoningContent(reqBody, h.reasoning); changed {
			reqBody = nb
		}
	}
	// 视频透传开关：默认关闭。关闭时请求含 video_url 直接 400——视频流量大，
	// 且多数模型不支持视频，需在系统设置显式开启才放行。
	if !h.cfg.Proxy.VideoPassThrough && containsVideoURL(body) {
		writeError(w, http.StatusBadRequest,
			"video_url 请求被拒绝：视频透传未开启。请在系统设置开启「视频透传」后再试",
			"invalid_request_error", "")
		return true, false, nil, 0, 0, 0, 0
	}
	// 流式请求强制注入 include_usage（无论客户端是否自带 stream_options），
	// 让上游返回 usage chunk 用于 token 统计与计费。
	if stream {
		if nb, serr := sjson.SetBytes(reqBody, "stream_options.include_usage", true); serr == nil {
			reqBody = nb
		}
	}

	target := strings.TrimRight(up.BaseURL, "/") + "/chat/completions"
	reqCtx := r.Context()
	if !stream {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(reqCtx, upstreamTimeoutFor(h.cfg))
		defer cancel()
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target, bytes.NewReader(reqBody))
	if err != nil {
		if last {
			writeError(w, http.StatusInternalServerError, "failed to build upstream request: "+err.Error(), "server_error", "")
			return true, false, nil, 0, 0, 0, 0
		}
		if h.fastFail != nil {
			h.fastFail.MarkFailedWithReason(up.Name, upstreamModel, "build request: "+err.Error())
		}
		return false, true, fmt.Errorf("build upstream request: %w", err), 0, 0, 0, 0
	}
	setUpstreamAuth(req, up)
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		if last {
			writeError(w, http.StatusBadGateway, "upstream request failed: "+err.Error(), "server_error", "upstream_unreachable")
			return true, false, nil, 0, 0, 0, 0
		}
		// 客户端断连（context.Canceled）不算上游故障，不标记 fastfail——
		// 否则一次断连会把所有候选全拉黑 60 分钟（2026-08-02 实测：6 个上游全被误拉黑）
		if !errors.Is(err, context.Canceled) && h.fastFail != nil {
			h.fastFail.MarkFailedWithReason(up.Name, upstreamModel, "request failed: "+err.Error())
		}
		return false, true, fmt.Errorf("upstream request failed: %w", err), 0, 0, 0, 0
	}
	defer resp.Body.Close()

	switch {
	case stream && resp.StatusCode >= 200 && resp.StatusCode < 300:
		// 流式一旦开始输出就不再切换，避免客户端收到半截流
		// 上游 2xx 即成功开始响应，必须清除 fastfail 黑名单
		// 否则后续流式请求一直跳不过冷却期，导致 free 层被永久跳过
		if h.fastFail != nil {
			h.fastFail.MarkSuccess(up.Name, upstreamModel)
		}
		streamInterrupted = h.streamCopy(w, resp, &promptTokens, &completionTokens, &promptCacheHitTokens, &promptCacheMissTokens)
		return true, false, nil, promptTokens, completionTokens, promptCacheHitTokens, promptCacheMissTokens
	case resp.StatusCode >= 400:
		// 读响应体用于关键词匹配
		respBody, _ := io.ReadAll(resp.Body)
		// 判断是否应重试
		shouldRetry := false
		for _, code := range h.cfg.Retry.RetryStatuses {
			if resp.StatusCode == code {
				shouldRetry = true
				break
			}
		}
		// 关键词匹配：状态码不在 retry_statuses 中但响应体含关键词
		if !shouldRetry {
			bodyStr := strings.ToLower(string(respBody))
			for _, kw := range h.cfg.Retry.RetryKeywords {
				if strings.Contains(bodyStr, strings.ToLower(kw)) {
					shouldRetry = true
					break
				}
			}
		}
		// 详细错误日志：记录上游名、状态码、请求体、上游错误体（均在 shouldRetry 判断后打，可重试+不可重试都覆盖）
		{
			respBodyStr := string(respBody)
			reqBodyStr := string(reqBody)
			const maxLen = 800
			if len(respBodyStr) > maxLen {
				respBodyStr = respBodyStr[:maxLen] + "...(truncated)"
			}
			if len(reqBodyStr) > maxLen {
				reqBodyStr = reqBodyStr[:maxLen] + "...(truncated)"
			}
			h.log.Warn("upstream 4xx/5xx detail",
				"upstream", up.Name,
				"model", model,
				"upstream_model", upstreamModel,
				"status", resp.StatusCode,
				"request_body", reqBodyStr,
				"upstream_body", respBodyStr)
		}
		if shouldRetry {
			// 返回可重试；带原因标记 fastfail：status + 响应体摘要（截断 500 字符防刷屏）
			if h.fastFail != nil {
				h.fastFail.MarkFailedWithReason(up.Name, upstreamModel,
					fmt.Sprintf("status=%d body=%s", resp.StatusCode, truncateLogStr(string(respBody), 500)))
			}
			return false, true, fmt.Errorf("upstream error: %s", resp.Status), 0, 0, 0, 0
		}
		// 不可重试，写错误到客户端（用已读取的 respBody 重建响应体）
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
		h.writeUpstreamError(w, resp)
		return true, false, fmt.Errorf("upstream error: %s", resp.Status), 0, 0, 0, 0
	default:
		if h.fastFail != nil {
			h.fastFail.MarkSuccess(up.Name, upstreamModel)
		}
		// 读取响应体用于 usage 解析，再整体透传
		respBody, rerr := io.ReadAll(resp.Body)
		if rerr != nil {
			h.log.Debug("read upstream body", "error", rerr)
		}
		// 空内容完成（思考型 max_tokens 不足被截断，content 空 + finish_reason=length）：
		// HTTP 200 但响应无效，非最后候选时切换下一个
		if IsEmptyCompletion(respBody) {
			if h.fastFail != nil {
				h.fastFail.MarkFailedWithReason(up.Name, upstreamModel, "empty completion")
			}
			return false, true, fmt.Errorf("empty completion (thinking truncated?)"), 0, 0, 0, 0
		}
		// 优先解析上游返回的 usage；缺失时用 tokenizer 估算
		if !parseUsage(respBody, &promptTokens, &completionTokens, &promptCacheHitTokens, &promptCacheMissTokens) && h.tokenizer != nil {
			promptTokens = int64(h.tokenizer.CountMessages(model, extractMessages(body)))
			completion := gjson.GetBytes(respBody, "choices.0.message.content").String()
			completionTokens = int64(h.tokenizer.Count(model, completion))
		}
		// 缓存 thinking 模式 tool_call 的 reasoning_content（供下一轮请求回传）
		if h.cacheReasoningEnabled() {
			cacheReasoningFromMessage(respBody, h.reasoning)
		}
		copyHeader(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		if _, werr := w.Write(respBody); werr != nil {
			h.log.Debug("write upstream body", "error", werr)
		}
		return true, false, nil, promptTokens, completionTokens, promptCacheHitTokens, promptCacheMissTokens
	}
}

// containsVideoURL 检测请求体 messages[].content 数组中是否含 video_url 类型的内容块。
// 支持 content 为数组（多模态）与纯字符串两种形态；含 video_url 返回 true。
func containsVideoURL(body []byte) bool {
	if !gjson.ValidBytes(body) {
		return false
	}
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return false
	}
	for _, msg := range messages.Array() {
		content := msg.Get("content")
		if !content.IsArray() {
			continue
		}
		for _, part := range content.Array() {
			if part.Get("type").String() == "video_url" {
				return true
			}
		}
	}
	return false
}

// isMultimodalRequest 判断请求是否含图像（多模态）：messages[].content 为数组且
// 含 type=image_url 或 type=image 的 part 即视为多模态。content 为纯字符串的
// 消息不算多模态（仅文本）。100% 自动检测，无需配置。
func isMultimodalRequest(body []byte) bool {
	if !gjson.ValidBytes(body) {
		return false
	}
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return false
	}
	for _, msg := range messages.Array() {
		content := msg.Get("content")
		if !content.IsArray() {
			continue // 字符串或缺失：不算多模态
		}
		for _, part := range content.Array() {
			t := part.Get("type").String()
			if t == "image_url" || t == "image" {
				return true
			}
		}
	}
	return false
}

// parseUsage 解析 OpenAI 响应体的 usage 字段，写入 promptTokens/completionTokens，
// 以及前缀缓存命中/未命中 token 数（promptCacheHit/promptCacheMiss 可为 nil，如 rerank/embed 不需要）。
// 返回 false 表示上游未返回 usage（调用方可回退到 tokenizer 估算）。
// 部分上游只回 total_tokens 时，计入 completion，保证 Tokens = prompt + completion 总量正确。
func parseUsage(data []byte, promptTokens, completionTokens, promptCacheHit, promptCacheMiss *int64) bool {
	usage := gjson.GetBytes(data, "usage")
	if !usage.Exists() {
		return false
	}
	*promptTokens = usage.Get("prompt_tokens").Int()
	*completionTokens = usage.Get("completion_tokens").Int()
	if *promptTokens == 0 && *completionTokens == 0 {
		if total := usage.Get("total_tokens").Int(); total > 0 {
			*completionTokens = total
		}
	}
	if promptCacheHit != nil {
		// 优先 DeepSeek 系字段 prompt_cache_hit_tokens，兜底 OpenAI 标准 prompt_tokens_details.cached_tokens
		*promptCacheHit = usage.Get("prompt_cache_hit_tokens").Int()
		if *promptCacheHit == 0 {
			*promptCacheHit = usage.Get("prompt_tokens_details.cached_tokens").Int()
		}
	}
	if promptCacheMiss != nil {
		*promptCacheMiss = usage.Get("prompt_cache_miss_tokens").Int()
		// 部分上游（商汤等 OpenAI 标准）只返回 cached_tokens，不返回 prompt_cache_miss_tokens。
		// 兜底：miss = prompt - hit，保证 hit+miss == prompt_tokens（命中率分母正确）。
		if *promptCacheMiss == 0 && *promptCacheHit > 0 && *promptTokens > *promptCacheHit {
			*promptCacheMiss = *promptTokens - *promptCacheHit
		}
		// 上游完全没有返回缓存字段时（如 agnes 首次请求无缓存可命中），
		// 按"全部未命中"记录（miss = prompt），日志命中率显示 0% 而非 '-'，
		// 与计费口径一致（calcCost 已有同样兜底）。
		if *promptCacheHit == 0 && *promptCacheMiss == 0 && *promptTokens > 0 {
			*promptCacheMiss = *promptTokens
		}
	}
	return true
}

// extractMessages 从请求体提取 messages 数组用于 token 估算。
// content 为对象数组（多模态）时降级拼接 text 字段。
func extractMessages(body []byte) []map[string]string {
	raw := gjson.GetBytes(body, "messages").Raw
	if raw == "" || raw == "null" {
		return nil
	}
	var msgs []map[string]string
	if err := json.Unmarshal([]byte(raw), &msgs); err == nil {
		return msgs
	}
	var out []map[string]string
	gjson.GetBytes(body, "messages").ForEach(func(_, m gjson.Result) bool {
		msg := map[string]string{"role": m.Get("role").String()}
		content := m.Get("content")
		if content.IsArray() {
			var sb strings.Builder
			content.ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").String() == "text" {
					sb.WriteString(part.Get("text").String())
				}
				return true
			})
			msg["content"] = sb.String()
		} else {
			msg["content"] = content.String()
		}
		out = append(out, msg)
		return true
	})
	return out
}

// streamCopy 将上游 SSE 响应边收边发地透传给客户端，同时解析 usage chunk 收集 token 统计
// 与前缀缓存命中/未命中 token 数（DeepSeek 等上游在 usage 里带 prompt_cache_hit/miss_tokens）。
// 处理 "usage":null 的中间 chunk（Exists() 对 null 也返回 true，需 IsObject() 过滤）。
// 返回 interrupted：客户端在流结束前断开（写响应失败），调用方应把日志状态记为 499
//（Nginx 语义 client closed request），避免把"中断"误记为 200 污染统计。
func (h *Handler) streamCopy(w http.ResponseWriter, resp *http.Response, promptTokens, completionTokens, promptCacheHit, promptCacheMiss *int64) (interrupted bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(resp.StatusCode)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	// thinking 模式 reasoning_content 缓存：累积 delta 分片，关联 tool_call_id；
	// 流结束（[DONE] 或 EOF）时写入最终完整内容（tool_calls 之后可能还有 reasoning 分片）
	cacheEnabled := h.cacheReasoningEnabled()
	var reasoningBuf strings.Builder
	var toolCallIDs []string
	for scanner.Scan() {
		line := scanner.Text()
		if _, werr := w.Write([]byte(line + "\n")); werr != nil {
			return true
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				// 流正常结束：补写最终累积的 reasoning（覆盖 tool_calls 之后的增量分片）
				if cacheEnabled && reasoningBuf.Len() > 0 && len(toolCallIDs) > 0 {
					h.reasoning.PutAll(toolCallIDs, reasoningBuf.String())
				}
				continue
			}
			if cacheEnabled {
				cacheReasoningDelta(data, &reasoningBuf, &toolCallIDs, h.reasoning)
			}
			usage := gjson.Get(data, "usage")
			// 只有真实 usage 对象才提取；null 中间 chunk 跳过
			if usage.Exists() && usage.IsObject() {
				if pt := usage.Get("prompt_tokens").Int(); pt > 0 {
					*promptTokens = pt
				}
				if ct := usage.Get("completion_tokens").Int(); ct > 0 {
					*completionTokens = ct
				}
				if pch := usage.Get("prompt_cache_hit_tokens").Int(); pch > 0 {
					*promptCacheHit = pch
				} else if cached := usage.Get("prompt_tokens_details.cached_tokens").Int(); cached > 0 {
					// OpenAI 标准字段兜底（商汤等上游用 cached_tokens 而非 prompt_cache_hit_tokens）
					*promptCacheHit = cached
				}
				if pcm := usage.Get("prompt_cache_miss_tokens").Int(); pcm > 0 {
					*promptCacheMiss = pcm
				} else if promptCacheHit != nil && *promptCacheHit > 0 && *promptTokens > *promptCacheHit {
					// OpenAI 标准上游不返回 miss 字段时，兜底 miss = prompt - hit
					*promptCacheMiss = *promptTokens - *promptCacheHit
				}
			}
		}
	}
	// 流可能没有 [DONE]（客户端中断/上游异常关闭）：EOF 时同样补写最终累积的 reasoning
	if cacheEnabled && reasoningBuf.Len() > 0 && len(toolCallIDs) > 0 {
		h.reasoning.PutAll(toolCallIDs, reasoningBuf.String())
	}
	return false
}

// writeUpstreamError 把上游的 4xx/5xx 响应映射为 OpenAI 标准错误格式。
func (h *Handler) writeUpstreamError(w http.ResponseWriter, resp *http.Response) {
	message := ""
	if data, err := io.ReadAll(resp.Body); err == nil {
		message = gjson.GetBytes(data, "error.message").String()
	}
	if message == "" {
		message = "upstream error: " + resp.Status
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		writeError(w, resp.StatusCode, message, "rate_limit_error", "rate_limit_exceeded")
	case resp.StatusCode >= 500:
		writeError(w, resp.StatusCode, message, "server_error", "upstream_error")
	case resp.StatusCode >= 400:
		writeError(w, resp.StatusCode, message, "invalid_request_error", "")
	default:
		writeError(w, resp.StatusCode, message, "server_error", "")
	}
}

// writeError 以 OpenAI 标准错误格式写响应。
func writeError(w http.ResponseWriter, status int, message, typ, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiError{Error: apiErrorDetail{
		Message: message,
		Type:    typ,
		Code:    code,
	}})
}

// truncateLogStr 截断字符串到 max 字节，超出时末尾加省略号标记。
// 用于日志字段（响应体摘要、错误信息等），防止超长内容刷屏。
func truncateLogStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

// hopByHopHeaders 是 HTTP/1.1 逐跳头，转发时不得复制。
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// copyHeader 复制上游响应头到客户端，跳过逐跳头与 Content-Length。
func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		if hopByHopHeaders[k] {
			continue
		}
		if k == "Content-Length" {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// statusRecorder 记录实际写出的状态码，供请求日志使用。
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

// WriteHeader 记录状态码并转发，只生效一次。
func (s *statusRecorder) WriteHeader(code int) {
	if s.wrote {
		return
	}
	s.wrote = true
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Write 未显式 WriteHeader 时按 200 处理。
func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.WriteHeader(http.StatusOK)
	}
	return s.ResponseWriter.Write(b)
}

// Rerank 处理 POST /v1/rerank，转发到 OpenAI 兼容上游（如硅基流动 bge-reranker）。
// 非流式 JSON POST；按 selectCandidates 统一优先级选择候选，失败自动切换。
func (h *Handler) Rerank(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w}
	start := time.Now()

	var model, upstream string
	defer func() {
		h.log.Info("rerank",
			"model", model,
			"upstream", upstream,
			"status", rec.status,
			"duration", time.Since(start).String(),
		)
	}()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(rec, http.StatusBadRequest, "failed to read request body", "invalid_request_error", "invalid_request")
		return
	}
	model = gjson.GetBytes(body, "model").String()
	if model == "" {
		writeError(rec, http.StatusBadRequest, "model is required", "invalid_request_error", "missing_model")
		return
	}

	upstreams, strategy, err := h.router.Route(model)
	if err != nil {
		writeError(rec, http.StatusNotFound, fmt.Sprintf("no upstream found for model %q", model), "invalid_request_error", "model_not_found")
		return
	}

	candidates := h.selectCandidates(upstreams, strategy, model)
	connIssues := false // 是否有连接类错误（网络断/超时），用于判断全局网络问题
	for i := 0; i < len(candidates); i++ {
		// 客户端已断开（context canceled）：不再尝试任何上游，也不标记 fastfail
		if r.Context().Err() != nil {
			h.log.Warn("client disconnected, aborting rerank retry loop",
				"model", model, "error", r.Context().Err())
			break
		}
		up := candidates[i]
		handled, retryable, ferr := h.forwardRerank(rec, r, body, up, model)
		if ferr != nil && h.health != nil {
			h.health.MarkFailure(up.Name)
		}
		if ferr != nil {
			var netErr net.Error
			if errors.As(ferr, &netErr) || errors.Is(ferr, context.DeadlineExceeded) {
				connIssues = true
			}
		}
		if handled {
			upstream = up.Name
			return
		}
		h.log.Warn("rerank upstream failed, trying next",
			"upstream", up.Name, "model", model, "error", ferr)
		if !retryable {
			break
		}
	}
	// 全部候选失败 + 含连接类错误 = 全局网络问题，清空黑名单让网络恢复后立即可用
	if connIssues && h.fastFail != nil {
		cleared := 0
		for _, up := range candidates {
			h.clearUpstreamBlacklist(up, model)
			cleared++
		}
		if cleared > 0 {
			h.log.Warn("all rerank upstreams failed with connection errors (likely network issue), cleared fastfail blacklist",
				"model", model, "cleared", cleared)
		}
	}
	writeError(rec, http.StatusBadGateway, "all upstreams failed", "server_error", "upstream_unreachable")
}

// forwardRerank 向单个上游转发一次 rerank 请求（OpenAI 兼容非流式）。
func (h *Handler) forwardRerank(w http.ResponseWriter, r *http.Request, body []byte, up *config.Upstream, model string) (handled, retryable bool, err error) {
	start := time.Now()
	var status int
	var promptTokens, completionTokens int64
	defer func() {
		if h.recorder == nil || !handled {
			return
		}
		if sr, ok := w.(*statusRecorder); ok {
			status = sr.status
		}
		h.recorder.Record(store.Record{
			Timestamp:        time.Now(),
			Upstream:         up.Name,
			Model:            model,
			Endpoint:         "rerank",
			Status:           status,
			DurationMS:       time.Since(start).Milliseconds(),
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			Tokens:           promptTokens + completionTokens,
			APIKey:           h.recordAPIKey(r),
			ClientAddr:       r.RemoteAddr, // 客户端地址 "IP:port"，用于区分调用程序
			UserAgent:        r.UserAgent(), // 客户端 UA，程序识别最强信号
		})
	}()

	reqBody := body
	upstreamModel := h.pickAvailableModel(up, model)
	if upstreamModel == "" {
		// fallback：全黑名单时按原 MapModel 随机选一个真实模型继续尝试（连接错误可清黑名单）
		upstreamModel = h.router.MapModel(up, model)
	}
	if upstreamModel != model {
		if reqBody, err = sjson.SetBytes(body, "model", upstreamModel); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to rewrite model field", "server_error", "")
			return true, false, nil
		}
	}

	target := strings.TrimRight(up.BaseURL, "/") + "/rerank"
	reqCtx, cancel := context.WithTimeout(r.Context(), upstreamTimeoutFor(h.cfg))
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target, bytes.NewReader(reqBody))
	if err != nil {
		return false, true, fmt.Errorf("build rerank request: %w", err)
	}
	setUpstreamAuth(req, up)
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		// 客户端断连（context.Canceled）不算上游故障，不标记 fastfail
		if !errors.Is(err, context.Canceled) && h.fastFail != nil {
			h.fastFail.MarkFailedWithReason(up.Name, upstreamModel, "request failed: "+err.Error())
		}
		return false, true, fmt.Errorf("rerank upstream request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		shouldRetry := false
		for _, code := range h.cfg.Retry.RetryStatuses {
			if resp.StatusCode == code {
				shouldRetry = true
				break
			}
		}
		if shouldRetry {
			if h.fastFail != nil {
				h.fastFail.MarkFailedWithReason(up.Name, upstreamModel, fmt.Sprintf("status=%d", resp.StatusCode))
			}
			return false, true, fmt.Errorf("rerank upstream error: %s", resp.Status)
		}
		h.writeUpstreamError(w, resp)
		return true, false, fmt.Errorf("rerank upstream error: %s", resp.Status)
	}
	if h.fastFail != nil {
		h.fastFail.MarkSuccess(up.Name, upstreamModel)
	}
	respBody, _ := io.ReadAll(resp.Body)
	parseUsage(respBody, &promptTokens, &completionTokens, nil, nil)
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
	return true, false, nil
}

// Embeddings 处理 POST /v1/embeddings，转发到 OpenAI 兼容上游。
// 按 selectCandidates 统一优先级选择候选，失败自动切换。
func (h *Handler) Embeddings(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w}
	start := time.Now()

	var model, upstream string
	defer func() {
		h.log.Info("embeddings",
			"model", model,
			"upstream", upstream,
			"status", rec.status,
			"duration", time.Since(start).String(),
		)
	}()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(rec, http.StatusBadRequest, "failed to read request body", "invalid_request_error", "invalid_request")
		return
	}
	model = gjson.GetBytes(body, "model").String()
	if model == "" {
		writeError(rec, http.StatusBadRequest, "model is required", "invalid_request_error", "missing_model")
		return
	}

	upstreams, strategy, err := h.router.Route(model)
	if err != nil {
		writeError(rec, http.StatusNotFound, fmt.Sprintf("no upstream found for model %q", model), "invalid_request_error", "model_not_found")
		return
	}

	candidates := h.selectCandidates(upstreams, strategy, model)
	connIssues := false // 是否有连接类错误（网络断/超时），用于判断全局网络问题
	for i := 0; i < len(candidates); i++ {
		// 客户端已断开（context canceled）：不再尝试任何上游，也不标记 fastfail
		if r.Context().Err() != nil {
			h.log.Warn("client disconnected, aborting embedding retry loop",
				"model", model, "error", r.Context().Err())
			break
		}
		up := candidates[i]
		handled, retryable, ferr := h.forwardEmbedding(rec, r, body, up, model)
		if ferr != nil && h.health != nil {
			h.health.MarkFailure(up.Name)
		}
		if ferr != nil {
			var netErr net.Error
			if errors.As(ferr, &netErr) || errors.Is(ferr, context.DeadlineExceeded) {
				connIssues = true
			}
		}
		if handled {
			upstream = up.Name
			return
		}
		h.log.Warn("embedding upstream failed, trying next",
			"upstream", up.Name, "model", model, "error", ferr)
		if !retryable {
			break
		}
	}
	// 全部候选失败 + 含连接类错误 = 全局网络问题，清空黑名单让网络恢复后立即可用
	if connIssues && h.fastFail != nil {
		cleared := 0
		for _, up := range candidates {
			h.clearUpstreamBlacklist(up, model)
			cleared++
		}
		if cleared > 0 {
			h.log.Warn("all embedding upstreams failed with connection errors (likely network issue), cleared fastfail blacklist",
				"model", model, "cleared", cleared)
		}
	}
	writeError(rec, http.StatusBadGateway, "all upstreams failed", "server_error", "upstream_unreachable")
}

// forwardEmbedding 向单个上游转发一次 embeddings 请求（OpenAI 兼容非流式）。
func (h *Handler) forwardEmbedding(w http.ResponseWriter, r *http.Request, body []byte, up *config.Upstream, model string) (handled, retryable bool, err error) {
	start := time.Now()
	var status int
	var promptTokens, completionTokens int64
	defer func() {
		if h.recorder == nil || !handled {
			return
		}
		if sr, ok := w.(*statusRecorder); ok {
			status = sr.status
		}
		h.recorder.Record(store.Record{
			Timestamp:        time.Now(),
			Upstream:         up.Name,
			Model:            model,
			Endpoint:         "embed",
			Status:           status,
			DurationMS:       time.Since(start).Milliseconds(),
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			Tokens:           promptTokens + completionTokens,
			APIKey:           h.recordAPIKey(r),
			ClientAddr:       r.RemoteAddr, // 客户端地址 "IP:port"，用于区分调用程序
			UserAgent:        r.UserAgent(), // 客户端 UA，程序识别最强信号
		})
	}()

	reqBody := body
	upstreamModel := h.pickAvailableModel(up, model)
	if upstreamModel == "" {
		// fallback：全黑名单时按原 MapModel 随机选一个真实模型继续尝试（连接错误可清黑名单）
		upstreamModel = h.router.MapModel(up, model)
	}
	if upstreamModel != model {
		if reqBody, err = sjson.SetBytes(body, "model", upstreamModel); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to rewrite model field", "server_error", "")
			return true, false, nil
		}
	}

	target := strings.TrimRight(up.BaseURL, "/") + "/embeddings"
	reqCtx, cancel := context.WithTimeout(r.Context(), upstreamTimeoutFor(h.cfg))
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target, bytes.NewReader(reqBody))
	if err != nil {
		return false, true, fmt.Errorf("build embedding request: %w", err)
	}
	setUpstreamAuth(req, up)
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		// 客户端断连（context.Canceled）不算上游故障，不标记 fastfail
		if !errors.Is(err, context.Canceled) && h.fastFail != nil {
			h.fastFail.MarkFailedWithReason(up.Name, upstreamModel, "request failed: "+err.Error())
		}
		return false, true, fmt.Errorf("embedding upstream request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		shouldRetry := false
		for _, code := range h.cfg.Retry.RetryStatuses {
			if resp.StatusCode == code {
				shouldRetry = true
				break
			}
		}
		if shouldRetry {
			if h.fastFail != nil {
				h.fastFail.MarkFailedWithReason(up.Name, upstreamModel, fmt.Sprintf("status=%d", resp.StatusCode))
			}
			return false, true, fmt.Errorf("embedding upstream error: %s", resp.Status)
		}
		h.writeUpstreamError(w, resp)
		return true, false, fmt.Errorf("embedding upstream error: %s", resp.Status)
	}
	if h.fastFail != nil {
		h.fastFail.MarkSuccess(up.Name, upstreamModel)
	}
	respBody, _ := io.ReadAll(resp.Body)
	parseUsage(respBody, &promptTokens, &completionTokens, nil, nil)
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
	return true, false, nil
}
