// Package admin 提供 Web 管理界面所需的只读 JSON API。
//
// 所有端点均为 GET，输出 application/json，不做鉴权（本地管理接口，
// 不挂 keys.Middleware）。仅提供只读查询，不做配置修改与热更新。
package admin

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"

	"github.com/icefairy/xuanji/internal/auth"
	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/health"
	"github.com/icefairy/xuanji/internal/proxy"
	"github.com/icefairy/xuanji/internal/router"
	"github.com/icefairy/xuanji/internal/store"
)

// Handler 提供管理端点的 HTTP 处理器集合。
type Handler struct {
	cfg    *config.Config
	hc     *health.Checker
	start  time.Time
	store  *store.Store         // nil 时 metrics 端点返回空数据
	reload func() error         // 热重载回调，nil 时不可用
	ff     *proxy.FastFailCache // 快速失败缓存；nil 时不显示 fast_fail 状态
	auth   *auth.APIKeys        // 下游 key 鉴权缓存；nil 时无需刷新
	px     *proxy.Handler       // 完整转发链路（对话调试 /admin/chat 用）；nil 时返回 503
	rt     *router.Router       // 路由探测器（对话调试 routing 展示用）；nil 时不展示路由信息

	quotaReload func() // 配额策略刷新回调（管理端改动组/分组后调用）；nil 时不刷新

	metricsCache    map[string]cacheEntry // 统计接口结果缓存（key=path?query）
	metricsCacheMu  sync.RWMutex
	metricsCacheTTL time.Duration
}

// SetAuth 注入下游 key 鉴权器（api_tokens CRUD 后刷新内存缓存）。
func (h *Handler) SetAuth(a *auth.APIKeys) { h.auth = a }

// refreshAuth 在 api_tokens 增删/启停后刷新鉴权缓存，避免新 key 立即 401 / 禁用 key 仍放行。
func (h *Handler) refreshAuth() {
	if h.auth != nil {
		h.auth.Refresh()
	}
}

// SetQuotaReload 设置配额策略刷新回调（main 注入 quota.Service.Refresh）。
func (h *Handler) SetQuotaReload(f func()) { h.quotaReload = f }

func (h *Handler) refreshQuota() {
	if h.quotaReload != nil {
		h.quotaReload()
	}
}

// upstreamTestTimeoutFor 返回单个上游直连测试端点的超时：
// 上游自身配置的 timeout（秒）优先，否则用全局 retry.upstream_timeout（秒），未配置时回退 30s。
func upstreamTestTimeoutFor(up *config.Upstream, cfg *config.Config) time.Duration {
	if up != nil && up.Timeout > 0 {
		return time.Duration(up.Timeout) * time.Second
	}
	if cfg != nil && cfg.Retry.UpstreamTimeout > 0 {
		return time.Duration(cfg.Retry.UpstreamTimeout) * time.Second
	}
	return 30 * time.Second
}

// New 基于配置与健康检查器构建管理 Handler。start 记录服务启动时刻，
// 供 /admin/status 计算 uptime。
func New(cfg *config.Config, hc *health.Checker) *Handler {
	return &Handler{
		cfg:             cfg,
		hc:              hc,
		start:           time.Now(),
		metricsCache:    map[string]cacheEntry{},
		metricsCacheTTL: 60 * time.Second,
	}
}

// SetFastFail 注入快速失败缓存（用于上游列表显示临时禁用状态）。
func (h *Handler) SetFastFail(ff *proxy.FastFailCache) { h.ff = ff }

// SetStore 注入 store 引用（nil 安全；metrics 端点无数据时返回空）。
func (h *Handler) SetStore(s *store.Store) { h.store = s }

// SetReload 设置热重载回调。
func (h *Handler) SetReload(fn func() error) { h.reload = fn }

// SetProxy 注入完整转发链路 handler 与路由探测器（对话调试 /admin/chat 用）。
// px 为 nil 时 /admin/chat 返回 503；rt 为 nil 时响应不含 upstream/upstream_model。
func (h *Handler) SetProxy(px *proxy.Handler, rt *router.Router) {
	h.px = px
	h.rt = rt
}

// statusResponse 是 GET /admin/status 的响应体。
// 注意：字段名不能叫 "status"，amis 会把响应 JSON 里 status 非 0 视为业务失败。
type statusResponse struct {
	Service          string `json:"service"`
	UptimeSeconds    int64  `json:"uptime_seconds"`
	Port             int    `json:"port"`
	UpstreamTotal    int    `json:"upstream_total"`
	UpstreamHealthy  int    `json:"upstream_healthy"`
	UpstreamDegraded int    `json:"upstream_degraded"`
	UpstreamDead     int    `json:"upstream_dead"`
	RulesTotal       int    `json:"rules_total"`
	DefaultStrategy  string `json:"default_strategy"`
}

// Status 返回服务概览：运行时长、端口、上游健康计数与规则总数。
// 上游按 hc.Status(name) 归类，StateUnknown 不计入任何一项。
func (h *Handler) Status(w http.ResponseWriter, _ *http.Request) {
	var healthy, degraded, dead int
	for _, up := range h.cfg.Upstreams {
		switch h.hc.Status(up.Name) {
		case health.StateHealthy:
			healthy++
		case health.StateDegraded:
			degraded++
		case health.StateDead:
			dead++
		}
	}
	writeJSON(w, statusResponse{
		Service:          "ok",
		UptimeSeconds:    int64(time.Since(h.start).Seconds()),
		Port:             h.cfg.Server.Port,
		UpstreamTotal:    len(h.cfg.Upstreams),
		UpstreamHealthy:  healthy,
		UpstreamDegraded: degraded,
		UpstreamDead:     dead,
		RulesTotal:       len(h.cfg.Routing.Rules),
		DefaultStrategy:  h.cfg.Routing.DefaultStrategy,
	})
}

// upstreamResponse 是 GET /admin/upstreams 的单个元素。
// 绝不包含 api_key 字段（安全）。
type upstreamResponse struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// Kind 能力类型：chat|emb|rerank|tts|asr|image（空=chat）。健康检查按此选探测端点。
	Kind            string   `json:"kind"`
	BaseURL         string   `json:"base_url"`
	APIKey          string   `json:"api_key"`
	Tier            string   `json:"tier"`
	Priority        int      `json:"priority"`
	Weight          int      `json:"weight"`
	Enabled         bool     `json:"enabled"`           // 1=启用 0=禁用（禁用的不参与转发）
	BillingExempt   bool     `json:"billing_exempt"`    // true=不参与计费（统计费用记 0，路由不受影响）
	Arrears         bool     `json:"arrears"`           // true=欠费标记中（停路由停健康检查，测试通过后自动清除）
	PerModelBilling bool     `json:"per_model_billing"` // true=模型独立计费（欠费按(上游,模型)粒度标记，仅跳该模型）
	FastFail        bool     `json:"fast_fail"`         // 快速失败黑名单中（后台探测可自动恢复）
	State           string   `json:"state"`
	LatencyMS       int64    `json:"latency_ms"`
	Models          []string `json:"models"`
	ModelCount      int      `json:"model_count"`
	ModelMapping    string   `json:"model_mapping"`    // JSON 对象字符串
	RequestOverride string   `json:"request_override"` // 请求体复写 JSON 字符串
	// Timeout 请求超时（秒，0=跟随全局 retry.upstream_timeout）。GET 必须回传，
	// 否则前端编辑表单回显 0，提交时 parseInt||0 会把已配置值误清零（2026-09-10 实测）。
	Timeout int `json:"timeout"`
}

// fastFailState 返回上游是否处于快速失败黑名单（渠道级判断，不区分模型）。
func (h *Handler) fastFailState(name string) bool {
	return h.ff != nil && h.ff.IsChannelBlacklisted(name)
}

// ruleResponse 是 GET /admin/rules 的单个元素。
type ruleResponse struct {
	Model     string   `json:"model"`
	Strategy  string   `json:"strategy"`
	Upstreams []string `json:"upstreams"`
	// FastFail 与 Upstreams 一一对应：该模型下每个上游的快速失败黑名单状态
	// （true=红/异常，false=绿/正常）。用于前端按状态着色。
	FastFail []bool `json:"fast_fail"`
	// Enabled 与 Upstreams 一一对应：上游是否启用（false=禁用，前端标红）。
	Enabled []bool `json:"enabled"`
	// HealthState 与 Upstreams 一一对应：上游健康检查状态（healthy/degraded/dead/unknown）。
	// 路由与统计状态不一致时，以此为准展示真实状态。
	HealthState []string `json:"health_state"`
	// Vision 该规则是否支持多模态（true=支持；请求带图时不做兑底）。
	Vision bool `json:"vision"`
	// VisionFallback 多模态兑底转发的聚合模型名（如 "flash"）；空=不兑底。
	VisionFallback string `json:"vision_fallback"`
}

// sortRuleUpstreams 按路由匹配顺序对规则的上游列表排序：
// 1. tier 升序（free→subscription→payg）
// 2. 同 tier 内 weight 降序
// 3. 不存在的上游排最后（最低优先级）
// 同步排序 fast_fail、enabled、health_state 数组，保持一一对应。
func sortRuleUpstreams(upstreams []string, fastFail []bool, enabled []bool, healthState []string, h *Handler) ([]string, []bool, []bool, []string) {
	n := len(upstreams)
	if n <= 1 {
		return upstreams, fastFail, enabled, healthState
	}
	// 复制一份，避免修改原切片
	names := make([]string, n)
	ff := make([]bool, n)
	en := make([]bool, n)
	hs := make([]string, n)
	copy(names, upstreams)
	if len(fastFail) == n {
		copy(ff, fastFail)
	}
	if len(enabled) == n {
		copy(en, enabled)
	}
	if len(healthState) == n {
		copy(hs, healthState)
	}

	// 排序索引
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(i, j int) bool {
		ti, tj := h.upstreamTierWeight(names[idx[i]]), h.upstreamTierWeight(names[idx[j]])
		if ti != tj {
			return ti < tj
		}
		wi, wj := h.upstreamWeight(names[idx[i]]), h.upstreamWeight(names[idx[j]])
		return wi > wj
	})

	outNames := make([]string, n)
	outFF := make([]bool, n)
	outEN := make([]bool, n)
	outHS := make([]string, n)
	for i, pos := range idx {
		outNames[i] = names[pos]
		outFF[i] = ff[pos]
		outEN[i] = en[pos]
		outHS[i] = hs[pos]
	}
	return outNames, outFF, outEN, outHS
}

// serverSummary 是 /admin/config 中 server 块的摘要（脱敏，无 api_key 明文）。
type serverSummary struct {
	Port int `json:"port"`
}

// endpointInfo 描述一个对外 HTTP 端点，供前端展示功能清单。
type endpointInfo struct {
	Path string `json:"path"`
	Desc string `json:"desc"`
}

// configResponse 是 GET /admin/config 的响应体。
type configResponse struct {
	Server          serverSummary  `json:"server"`
	DefaultStrategy string         `json:"default_strategy"`
	Endpoints       []endpointInfo `json:"endpoints"`
}

// writeJSON 以 application/json 输出 v。
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

type cacheEntry struct {
	data []byte
	exp  time.Time
}

// metricsSummaryResponse 是 GET /admin/metrics/summary 的响应体。
type metricsSummaryResponse struct {
	TotalRequests   int64   `json:"total_requests"`
	TotalSuccesses  int64   `json:"total_successes"`
	SuccessRate     float64 `json:"success_rate"`
	TotalTokens     int64   `json:"total_tokens"`
	AvgLatencyMS    float64 `json:"avg_latency_ms"`
	ActiveUpstreams int     `json:"active_upstreams"`
}

// metricsSince 解析请求时间范围参数，返回 RFC3339 起始时间字符串。
// 空串表示不过滤（全部）。默认近 7 天。
func metricsSince(r *http.Request) string {
	now := time.Now()
	loc := time.FixedZone("CST", 8*3600)
	switch r.URL.Query().Get("range") {
	case "today":
		n := now.In(loc)
		return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc).UTC().Format(time.RFC3339)
	case "3d":
		return now.AddDate(0, 0, -3).Format(time.RFC3339)
	case "7d":
		return now.AddDate(0, 0, -7).Format(time.RFC3339)
	case "30d":
		return now.AddDate(0, 0, -30).Format(time.RFC3339)
	case "all":
		return "" // 全部不过滤
	default:
		return now.AddDate(0, 0, -7).Format(time.RFC3339) // 默认近 7 天
	}
}

// metricsRangeStr 返回 range 参数，默认 7d（与前端一致）。
func metricsRangeStr(r *http.Request) string {
	if v := r.URL.Query().Get("range"); v != "" {
		return v
	}
	return "7d"
}

// rangeStart 返回某 range 的起始东八区时间（用于趋势图补零）。
func rangeStart(rangeStr string, loc *time.Location) time.Time {
	now := time.Now().In(loc)
	switch rangeStr {
	case "today":
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	case "3d":
		return now.AddDate(0, 0, -3)
	case "7d":
		return now.AddDate(0, 0, -7)
	case "30d":
		return now.AddDate(0, 0, -30)
	default: // all 或未知：返回很早，由调用方从最早数据日覆盖
		return now.AddDate(0, 0, -3650)
	}
}

// dimToCostRows 把维度聚合 map 转成费用接口所需的 CostRow 列表（按 cost 降序）。
func dimToCostRows(m map[string]*store.DimStat) []store.CostRow {
	out := make([]store.CostRow, 0, len(m))
	for name, d := range m {
		if d == nil {
			continue
		}
		out = append(out, store.CostRow{Name: name, Cost: d.Cost, Requests: int(d.Requests), Tokens: d.Tokens})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cost > out[j].Cost })
	return out
}

// upstreamMetrics 是 GET /admin/metrics/upstreams 的单个元素。
type upstreamMetrics struct {
	Name           string  `json:"name"`
	Requests       int64   `json:"requests"`
	Successes      int64   `json:"successes"`
	Failures       int64   `json:"failures"`
	SuccessRate    float64 `json:"success_rate"`
	AvgLatencyMS   float64 `json:"avg_latency_ms"`
	AvgTTFTMS      float64 `json:"avg_ttft_ms"`     // 平均首 token 时间（毫秒）
	TokensPerSec   float64 `json:"tokens_per_sec"`  // 平均每秒生成 token 数
	ThinkingTokens int64   `json:"thinking_tokens"` // 思考 token 总量（DeepSeek thinking_tokens / OpenAI reasoning_tokens）
	TotalTokens    int64   `json:"total_tokens"`
	State          string  `json:"state"` // healthy / degraded / dead（来自健康检查）
	// 定时探测统计：健康度 = ProbeSuccess / (ProbeSuccess + ProbeFail)
	ProbeSuccess int64   `json:"probe_success"`
	ProbeFail    int64   `json:"probe_fail"`
	HealthRate   float64 `json:"health_rate"` // 探测成功率 0~1；无探测数据时 0
}

// hourlyBucket 是 GET /admin/metrics/hourly 的单个元素。
type hourlyBucket struct {
	Hour      string `json:"hour"`
	Requests  int64  `json:"requests"`
	Successes int64  `json:"successes"`
	Tokens    int64  `json:"tokens"`
}

// dailyBucket 是 GET /admin/metrics/daily 的单个元素（按天聚合）。
type dailyBucket struct {
	Date      string `json:"date"`
	Requests  int64  `json:"requests"`
	Successes int64  `json:"successes"`
	Tokens    int64  `json:"tokens"`
}

// apiKeyMetrics 是 GET /admin/metrics/keys 的单个元素（按下游 API Key 聚合）。
type apiKeyMetrics struct {
	Name         string  `json:"name"`
	Requests     int64   `json:"requests"`
	Successes    int64   `json:"successes"`
	SuccessRate  float64 `json:"success_rate"`
	AvgLatencyMS float64 `json:"avg_latency_ms"`
	TotalTokens  int64   `json:"total_tokens"`
	CacheHit     int64   `json:"cache_hit_tokens"`
	CacheMiss    int64   `json:"cache_miss_tokens"`
}

// APIKeyModelUsage 是单个 API Key 的模型使用分布。
type APIKeyModelUsage struct {
	Model string `json:"model"`
	Count int64  `json:"count"`
}

// fmtCST 把 RFC3339（UTC）转成东八区标准时间 "2006-01-02 15:04:05"。
// 解析失败时原样返回（老数据/异常数据不炸页面）。
func fmtCST(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	return t.In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04:05")
}

// generateToken 生成随机 API key（32 字节 hex）。
func generateToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// upstreamByConfig 从内存配置找上游；h.store 非 nil 时优先从数据库读取
// configBool 读取 config 表布尔键（未配置/读取失败视为 false）。
func (h *Handler) configBool(key string) bool {
	if h.store == nil {
		return false
	}
	v, err := h.store.GetConfig(key)
	if err != nil {
		return false
	}
	return strings.TrimSpace(v) == "true" || strings.TrimSpace(v) == "1"
}

// （数据库是配置唯一来源，内存 cfg 可能因直改 DB 未 reload 而过期）。
func (h *Handler) upstreamByName(name string) *config.Upstream {
	if h.store != nil {
		rows, err := h.store.ListUpstreams()
		if err == nil {
			for i := range rows {
				u := &rows[i]
				if u.Name == name {
					return &config.Upstream{
						Name:            u.Name,
						Type:            u.Type,
						BaseURL:         u.BaseURL,
						APIKey:          u.APIKey,
						Tier:            u.Tier,
						Priority:        u.Priority,
						Weight:          u.Weight,
						Models:          parseStringSlice(u.Models),
						ModelMapping:    parseStringMap(u.ModelMapping),
						Timeout:         u.Timeout,
						Arrears:         u.Arrears == 1,
						PerModelBilling: h.configBool("upstream." + u.Name + ".per_model_billing"),
					}
				}
			}
		}
	}
	for i := range h.cfg.Upstreams {
		if h.cfg.Upstreams[i].Name == name {
			return &h.cfg.Upstreams[i]
		}
	}
	return nil
}

// parseStringSlice 解析 JSON 数组字符串。
func parseStringSlice(s string) []string {
	return config.ParseModelsString(s)
}

// parseStringMap 解析 JSON 对象字符串。
func parseStringMap(s string) map[string]string {
	out := make(map[string]string)
	if s == "" {
		return out
	}
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

// truncateStr 截断长字符串。
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// costMetricsResponse 是 GET /admin/metrics/cost 的响应。
type costMetricsResponse struct {
	TotalCost  float64         `json:"total_cost"` // 时间段内总费用（元）
	ByUpstream []store.CostRow `json:"by_upstream"`
	ByAPIKey   []store.CostRow `json:"by_api_key"`
	ByModel    []store.CostRow `json:"by_model"`
}

// chatRequest 是 POST /admin/chat 的请求体（对话调试）。
type chatRequest struct {
	Model             string        `json:"model"`
	Messages          []chatMessage `json:"messages"`
	Images            []chatImage   `json:"images"` // 可选；data 不含 data: 前缀
	Temperature       float64       `json:"temperature"`
	TopP              float64       `json:"top_p"`
	TopK              int           `json:"top_k"`              // 0 表示不传
	RepetitionPenalty float64       `json:"repetition_penalty"` // 0 表示不传
	FrequencyPenalty  float64       `json:"frequency_penalty"`  // 0 表示不传
	MaxTokens         int           `json:"max_tokens"`
	ReasoningEffort   string        `json:"reasoning_effort"` // 空串表示不传；none/low/medium/high/max
}

// chatMessage 是对话调试的单个消息。
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatImage 是对话调试的图片附件（OpenAI 标准 data URL base64）。
type chatImage struct {
	Name string `json:"name"`
	Data string `json:"data"` // base64，不含 data: 前缀
}

// buildChatMessages 把 images 拼进最后一条 user 消息的 content（OpenAI 多模态数组）。
// 返回改造后的消息列表与"是否多模态请求"（供路由探测）。无图片时原样返回。
func buildChatMessages(messages []chatMessage, images []chatImage) ([]map[string]any, bool) {
	out := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		out = append(out, map[string]any{"role": m.Role, "content": m.Content})
	}
	if len(images) == 0 {
		return out, false
	}
	idx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			idx = i
			break
		}
	}
	if idx < 0 {
		// 没有 user 消息：追加一条空 user 消息承载图片
		out = append(out, map[string]any{"role": "user", "content": ""})
		idx = len(out) - 1
	}
	parts := []map[string]any{{"type": "text", "text": messages[idx].Content}}
	for _, img := range images {
		parts = append(parts, map[string]any{
			"type": "image_url",
			"image_url": map[string]string{
				"url": "data:" + mimeForImage(img.Name) + ";base64," + img.Data,
			},
		})
	}
	out[idx]["content"] = parts
	return out, true
}

// mimeForImage 按图片文件名扩展名映射 MIME 类型；未知扩展名回退 image/png。
func mimeForImage(name string) string {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(name), ".")) {
	case "jpg", "jpeg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	case "gif":
		return "image/gif"
	default:
		return "image/png"
	}
}

// extractChatReply 从 OpenAI chat 响应提取模型回复文本（content 可能为数组，拼接 text 部分）。
// 2026-08-26 增强：同时提取思考内容（兼容 reasoning_content / reasoning /
// thinking_content / thinking 等上游字段名），并按「thinking + response」包裹返回，
// 供对话调试页直观观察模型是否真实产出思考（deepseek-v4-flash 等思考型模型）。
// 无思考时输出保持原状（纯内容）。
func extractChatReply(data []byte) string {
	msg := gjson.GetBytes(data, "choices.0.message")
	// 思考部分：按常见字段名依次尝试，取第一个非空
	thinking := ""
	for _, k := range []string{"reasoning_content", "reasoning", "thinking_content", "thinking"} {
		if v := msg.Get(k).String(); v != "" {
			thinking = v
			break
		}
	}
	content := msg.Get("content")
	var text string
	if content.IsArray() {
		var sb strings.Builder
		content.ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").String() == "text" {
				sb.WriteString(part.Get("text").String())
			}
			return true
		})
		text = sb.String()
	} else {
		text = content.String()
	}
	if thinking == "" {
		return text
	}
	// 用户约定格式：thinking 换行包裹思考，再换行显示 response 内容
	var sb strings.Builder
	sb.WriteString("thinking\n")
	sb.WriteString(thinking)
	if text != "" {
		sb.WriteString("\n\nresponse\n")
		sb.WriteString(text)
	} else {
		sb.WriteString("\n\nresponse\n（上游未返回内容——思考写完即结束或 max_tokens 被思考耗尽）")
	}
	return sb.String()
}

// extractChatUsage 从 OpenAI chat 响应提取 token 用量（含前缀缓存命中/未命中）。
func extractChatUsage(data []byte) map[string]int64 {
	usage := map[string]int64{}
	u := gjson.GetBytes(data, "usage")
	if !u.Exists() {
		return usage
	}
	prompt := u.Get("prompt_tokens").Int()
	completion := u.Get("completion_tokens").Int()
	hit := u.Get("prompt_cache_hit_tokens").Int()
	if hit == 0 {
		// OpenAI 标准字段兑底（商汤等上游用 prompt_tokens_details.cached_tokens）
		hit = u.Get("prompt_tokens_details.cached_tokens").Int()
	}
	miss := u.Get("prompt_cache_miss_tokens").Int()
	if miss == 0 && hit > 0 && prompt > hit {
		miss = prompt - hit
	}
	usage["prompt_tokens"] = prompt
	usage["completion_tokens"] = completion
	usage["cache_hit_tokens"] = hit
	usage["cache_miss_tokens"] = miss
	return usage
}

// captureWriter 捕获 proxy 转发链路的响应状态码与响应体（对话调试用）。
type captureWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *captureWriter) Header() http.Header { return w.header }

func (w *captureWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}

func (w *captureWriter) Write(b []byte) (int, error) { return w.body.Write(b) }

// groupQuotaView 组视图里一个模型行的“限额 + 组内已用”。
type groupQuotaView struct {
	Model      string `json:"model"`
	Token5H    int64  `json:"token_5h"`
	TokenWeek  int64  `json:"token_week"`
	TokenMonth int64  `json:"token_month"`
	Used5H     int64  `json:"used_5h"` // 组内全部 key 对应用量（组视图统计）
	UsedWeek   int64  `json:"used_week"`
	UsedMonth  int64  `json:"used_month"`
}

// groupView 组列表单个元素。
type groupView struct {
	ID            uint             `json:"id"`
	Name          string           `json:"name"`
	AllowedModels string           `json:"allowed_models"`
	Remark        string           `json:"remark"`
	MemberCount   int              `json:"member_count"`
	Quotas        []groupQuotaView `json:"quotas"`
}
