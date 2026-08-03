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
	recorder  *store.Recorder // 指标记录器；nil 时跳过记录
	fastFail  *FastFailCache  // 快速失败缓存；nil 时不启用
	tokenizer *Tokenizer      // token 计数器；nil 时跳过估算回退
	discounts []store.Discount // 渠道优惠时段，用于同 tier 同 weight 内折扣优先排序
	keyName   func(r *http.Request) string // 下游 API Key 展示名（统计用）；nil 时记录空
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
//   不判空、不切换候选。思考字段名因推理引擎而异，必须同时检查两种：
//   - `reasoning_content`：DeepSeek R1/V4、Kimi、GLM、MiniMax、vLLM(Qwen3 Thinking) 等主流 OpenAI 兼容格式
//   - `reasoning`：商汤日日新等部分厂商（2026-08 实测确认）
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
			return
		}
		h.log.Warn("upstream failed, trying next",
			"upstream", up.Name, "model", model, "error", ferr)
		if !retryable {
			break
		}
		retryCount++
		// 循环完所有候选后，如果 retryCount < maxRetries，从头开始（再试一轮候选）
		if i == len(candidates)-1 && retryCount < maxRetries {
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
			if h.fastFail.IsBlacklisted(up.Name, model) {
				h.fastFail.MarkSuccess(up.Name, model)
				cleared++
			}
		}
		if cleared > 0 {
			h.log.Warn("all upstreams failed with connection errors (likely network issue), cleared fastfail blacklist",
				"model", model, "cleared", cleared)
		}
	}
	writeError(rec, http.StatusBadGateway, "all upstreams failed", "server_error", "upstream_unreachable")
}

// selectCandidates 依据健康状态与统一优先级选出转发候选：
// 1. 禁用/快速失败/不健康 过滤（disabled 不参与；全挂时回退第一个）
// 2. tier 永远优先（免费 > 包月 > 按量）——成本铁律
// 3. 同 tier 内按 weight 降序（权重高优先）
// 4. 同 tier 同 weight 内：当前处于优惠时段的上游优先
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
	// 快速失败过滤：跳过冷却期内的上游（按渠道+模型两级粒度判断）
	if h.fastFail != nil {
		var filtered []*config.Upstream
		for _, u := range ups {
			if !h.fastFail.IsBlacklisted(u.Name, model) {
				filtered = append(filtered, u)
			}
		}
		if len(filtered) > 0 {
			ups = filtered
		} // 全部被黑名单时不缩减，fallback 到原有逻辑
	}
	if h.health != nil {
		healthy := h.health.HealthyUpstreams(ups)
		if len(healthy) == 0 {
			// 兜底：全都不健康时也不能乱选——按 tier 升序（free→subscription→payg），
			// 让收费渠道永远排最后。直接 ups[:1] 会返回规则顺序第一个（往往是 payg）。
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

	// 按 tier 分组：free(0) < subscription(1) < payg(2)
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
			Endpoint:         "chat",
			Status:           status,
			DurationMS:       time.Since(start).Milliseconds(),
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			Tokens:           promptTokens + completionTokens,
			APIKey:           h.recordAPIKey(r),
			PromptCacheHitTokens:  promptCacheHitTokens,
			PromptCacheMissTokens: promptCacheMissTokens,
		})
	}()
	reqBody := body
	upstreamModel := model
	if mapped := h.router.MapModel(up, model); mapped != model {
		upstreamModel = mapped
		if reqBody, err = sjson.SetBytes(body, "model", mapped); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to rewrite model field", "server_error", "")
			return true, false, nil, 0, 0, 0, 0
		}
	}
	// 思考深度归一化：客户端标准 reasoning_effort → 目标模型实际思考参数
	// （DeepSeek 透传/适配档位、商汤转 output_config.effort、Kimi/GLM 转 thinking.type 等）
	if nb, changed := normalizeThinkingEffort(reqBody, upstreamModel); changed {
		reqBody = nb
	}
	// 流式请求注入 include_usage，让上游返回 usage chunk（用于 token 统计）
	if stream && !gjson.GetBytes(reqBody, "stream_options").Exists() {
		if nb, serr := sjson.SetBytes(reqBody, "stream_options", map[string]bool{"include_usage": true}); serr == nil {
			reqBody = nb
		}
	}

	target := strings.TrimRight(up.BaseURL, "/")
	// 如果 base_url 末尾已有 /v1，不重复添加
	if !strings.HasSuffix(target, "/v1") {
		target += chatPath
	} else {
		target += "/chat/completions"
	}
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
			h.fastFail.MarkFailed(up.Name, model)
		}
		return false, true, fmt.Errorf("build upstream request: %w", err), 0, 0, 0, 0
	}
	req.Header.Set("Authorization", "Bearer "+up.APIKey)
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
			h.fastFail.MarkFailed(up.Name, model)
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
			h.fastFail.MarkSuccess(up.Name, model)
		}
		h.streamCopy(w, resp, &promptTokens, &completionTokens, &promptCacheHitTokens, &promptCacheMissTokens)
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
		if shouldRetry {
			// 返回可重试
			if h.fastFail != nil {
				h.fastFail.MarkFailed(up.Name, model)
			}
			return false, true, fmt.Errorf("upstream error: %s", resp.Status), 0, 0, 0, 0
		}
		// 不可重试，写错误到客户端（用已读取的 respBody 重建响应体）
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
		h.writeUpstreamError(w, resp)
		return true, false, fmt.Errorf("upstream error: %s", resp.Status), 0, 0, 0, 0
	default:
		if h.fastFail != nil {
			h.fastFail.MarkSuccess(up.Name, model)
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
				h.fastFail.MarkFailed(up.Name, model)
			}
			return false, true, fmt.Errorf("empty completion (thinking truncated?)"), 0, 0, 0, 0
		}
		// 优先解析上游返回的 usage；缺失时用 tokenizer 估算
		if !parseUsage(respBody, &promptTokens, &completionTokens, &promptCacheHitTokens, &promptCacheMissTokens) && h.tokenizer != nil {
			promptTokens = int64(h.tokenizer.CountMessages(model, extractMessages(body)))
			completion := gjson.GetBytes(respBody, "choices.0.message.content").String()
			completionTokens = int64(h.tokenizer.Count(model, completion))
		}
		copyHeader(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		if _, werr := w.Write(respBody); werr != nil {
			h.log.Debug("write upstream body", "error", werr)
		}
		return true, false, nil, promptTokens, completionTokens, promptCacheHitTokens, promptCacheMissTokens
	}
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
		*promptCacheHit = usage.Get("prompt_cache_hit_tokens").Int()
	}
	if promptCacheMiss != nil {
		*promptCacheMiss = usage.Get("prompt_cache_miss_tokens").Int()
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
func (h *Handler) streamCopy(w http.ResponseWriter, resp *http.Response, promptTokens, completionTokens, promptCacheHit, promptCacheMiss *int64) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(resp.StatusCode)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if _, werr := w.Write([]byte(line + "\n")); werr != nil {
			return
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				continue
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
				}
				if pcm := usage.Get("prompt_cache_miss_tokens").Int(); pcm > 0 {
					*promptCacheMiss = pcm
				}
			}
		}
	}
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
			if h.fastFail.IsBlacklisted(up.Name, model) {
				h.fastFail.MarkSuccess(up.Name, model)
				cleared++
			}
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
		})
	}()

	reqBody := body
	if mapped := h.router.MapModel(up, model); mapped != model {
		if reqBody, err = sjson.SetBytes(body, "model", mapped); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to rewrite model field", "server_error", "")
			return true, false, nil
		}
	}

	target := strings.TrimRight(up.BaseURL, "/")
	if !strings.HasSuffix(target, "/v1") {
		target += "/v1/rerank"
	} else {
		target += "/rerank"
	}
	reqCtx, cancel := context.WithTimeout(r.Context(), upstreamTimeoutFor(h.cfg))
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target, bytes.NewReader(reqBody))
	if err != nil {
		return false, true, fmt.Errorf("build rerank request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+up.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		// 客户端断连（context.Canceled）不算上游故障，不标记 fastfail
		if !errors.Is(err, context.Canceled) && h.fastFail != nil {
			h.fastFail.MarkFailed(up.Name, model)
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
				h.fastFail.MarkFailed(up.Name, model)
			}
			return false, true, fmt.Errorf("rerank upstream error: %s", resp.Status)
		}
		h.writeUpstreamError(w, resp)
		return true, false, fmt.Errorf("rerank upstream error: %s", resp.Status)
	}
	if h.fastFail != nil {
		h.fastFail.MarkSuccess(up.Name, model)
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
			if h.fastFail.IsBlacklisted(up.Name, model) {
				h.fastFail.MarkSuccess(up.Name, model)
				cleared++
			}
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
		})
	}()

	reqBody := body
	if mapped := h.router.MapModel(up, model); mapped != model {
		if reqBody, err = sjson.SetBytes(body, "model", mapped); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to rewrite model field", "server_error", "")
			return true, false, nil
		}
	}

	target := strings.TrimRight(up.BaseURL, "/")
	if !strings.HasSuffix(target, "/v1") {
		target += "/v1/embeddings"
	} else {
		target += "/embeddings"
	}
	reqCtx, cancel := context.WithTimeout(r.Context(), upstreamTimeoutFor(h.cfg))
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target, bytes.NewReader(reqBody))
	if err != nil {
		return false, true, fmt.Errorf("build embedding request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+up.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		// 客户端断连（context.Canceled）不算上游故障，不标记 fastfail
		if !errors.Is(err, context.Canceled) && h.fastFail != nil {
			h.fastFail.MarkFailed(up.Name, model)
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
				h.fastFail.MarkFailed(up.Name, model)
			}
			return false, true, fmt.Errorf("embedding upstream error: %s", resp.Status)
		}
		h.writeUpstreamError(w, resp)
		return true, false, fmt.Errorf("embedding upstream error: %s", resp.Status)
	}
	if h.fastFail != nil {
		h.fastFail.MarkSuccess(up.Name, model)
	}
	respBody, _ := io.ReadAll(resp.Body)
	parseUsage(respBody, &promptTokens, &completionTokens, nil, nil)
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
	return true, false, nil
}
