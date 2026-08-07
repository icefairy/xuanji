// Package admin 提供 Web 管理界面所需的只读 JSON API。
//
// 所有端点均为 GET，输出 application/json，不做鉴权（本地管理接口，
// 不挂 keys.Middleware）。仅提供只读查询，不做配置修改与热更新。
package admin

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/icefairy/xuanji/internal/auth"
	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/health"
	"github.com/icefairy/xuanji/internal/proxy"
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
	ana    *ClientAnalyzer      // 客户端程序分析器；nil 时分析端点返回错误
}

// SetAuth 注入下游 key 鉴权器（api_tokens CRUD 后刷新内存缓存）。
func (h *Handler) SetAuth(a *auth.APIKeys) { h.auth = a }

// refreshAuth 在 api_tokens 增删/启停后刷新鉴权缓存，避免新 key 立即 401 / 禁用 key 仍放行。
func (h *Handler) refreshAuth() {
	if h.auth != nil {
		h.auth.Refresh()
	}
}

// upstreamTestTimeout 返回上游直连测试端点的超时（retry.upstream_timeout 秒），未配置时 30s。
func upstreamTestTimeout(cfg *config.Config) time.Duration {
	if cfg != nil && cfg.Retry.UpstreamTimeout > 0 {
		return time.Duration(cfg.Retry.UpstreamTimeout) * time.Second
	}
	return 30 * time.Second
}

// New 基于配置与健康检查器构建管理 Handler。start 记录服务启动时刻，
// 供 /admin/status 计算 uptime。
func New(cfg *config.Config, hc *health.Checker) *Handler {
	return &Handler{cfg: cfg, hc: hc, start: time.Now()}
}

// SetFastFail 注入快速失败缓存（用于上游列表显示临时禁用状态）。
func (h *Handler) SetFastFail(ff *proxy.FastFailCache) { h.ff = ff }

// SetStore 注入 store 引用（nil 安全；metrics 端点无数据时返回空）。
func (h *Handler) SetStore(s *store.Store) { h.store = s }

// SetReload 设置热重载回调。
func (h *Handler) SetReload(fn func() error) { h.reload = fn }

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
	Name         string   `json:"name"`
	Type         string   `json:"type"`
	BaseURL      string   `json:"base_url"`
	APIKey       string   `json:"api_key"`
	Tier         string   `json:"tier"`
	Priority     int      `json:"priority"`
	Weight       int      `json:"weight"`
	Enabled      bool     `json:"enabled"`   // 1=启用 0=禁用（禁用的不参与转发）
	FastFail     bool     `json:"fast_fail"` // 快速失败黑名单中（后台探测可自动恢复）
	State        string   `json:"state"`
	LatencyMS    int64    `json:"latency_ms"`
	Models       []string `json:"models"`
	ModelCount   int      `json:"model_count"`
	ModelMapping string   `json:"model_mapping"` // JSON 对象字符串
}

// fastFailState 返回上游是否处于快速失败黑名单（渠道级判断，不区分模型）。
func (h *Handler) fastFailState(name string) bool {
	return h.ff != nil && h.ff.IsChannelBlacklisted(name)
}

// Upstreams 返回上游列表。优先从数据库读取（h.store 非 nil），否则从配置读取。
func (h *Handler) Upstreams(w http.ResponseWriter, _ *http.Request) {
	if h.store != nil {
		rows, err := h.store.ListUpstreams()
		if err == nil {
			resp := make([]upstreamResponse, 0, len(rows))
			for _, u := range rows {
				models := parseStringSlice(u.Models)
				resp = append(resp, upstreamResponse{
					Name:         u.Name,
					Type:         u.Type,
					BaseURL:      u.BaseURL,
					APIKey:       u.APIKey,
					Tier:         u.Tier,
					Priority:     u.Priority,
					Weight:       u.Weight,
					Enabled:      u.Enabled == 1,
					FastFail:     h.fastFailState(u.Name),
					State:        string(h.hc.Status(u.Name)),
					LatencyMS:    h.hc.Latency(u.Name).Milliseconds(),
					Models:       models,
					ModelCount:   len(models),
					ModelMapping: u.ModelMapping,
				})
			}
			writeJSON(w, resp)
			return
		}
	}
	// fallback: 从静态配置读取
	resp := make([]upstreamResponse, 0, len(h.cfg.Upstreams))
	for _, up := range h.cfg.Upstreams {
		mm, _ := json.Marshal(up.ModelMapping)
		resp = append(resp, upstreamResponse{
			Name:         up.Name,
			Type:         up.Type,
			BaseURL:      up.BaseURL,
			APIKey:       up.APIKey,
			Tier:         up.Tier,
			Priority:     up.Priority,
			Weight:       up.Weight,
			Enabled:      up.Enabled,
			FastFail:     h.fastFailState(up.Name),
			State:        string(h.hc.Status(up.Name)),
			LatencyMS:    h.hc.Latency(up.Name).Milliseconds(),
			Models:       up.Models,
			ModelCount:   len(up.Models),
			ModelMapping: string(mm),
		})
	}
	writeJSON(w, resp)
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
}

// upstreamEnabled 返回上游是否启用；未找到时返回 false（视为异常）。
func (h *Handler) upstreamEnabled(name string) bool {
	if h.cfg == nil {
		return false
	}
	for i := range h.cfg.Upstreams {
		if h.cfg.Upstreams[i].Name == name {
			return h.cfg.Upstreams[i].Enabled
		}
	}
	return false
}

// upstreamTierWeight 返回上游的 tier 权重（0=free, 1=subscription, 2=payg）；未找到时返回 3（最低优先级）。
func (h *Handler) upstreamTierWeight(name string) int {
	if h.cfg == nil {
		return 3
	}
	for i := range h.cfg.Upstreams {
		if h.cfg.Upstreams[i].Name == name {
			return h.cfg.Upstreams[i].TierWeight()
		}
	}
	return 3
}

// upstreamWeight 返回上游的 weight；未找到时返回 0。
func (h *Handler) upstreamWeight(name string) int {
	if h.cfg == nil {
		return 0
	}
	for i := range h.cfg.Upstreams {
		if h.cfg.Upstreams[i].Name == name {
			return h.cfg.Upstreams[i].Weight
		}
	}
	return 0
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

// ruleFastFail 返回 rule 每个上游在指定模型下的黑名单状态（nil 表示未启用 fastfail）。
func (h *Handler) ruleFastFail(model string, upstreams []string) []bool {
	if h.ff == nil {
		return nil
	}
	out := make([]bool, len(upstreams))
	for i, name := range upstreams {
		out[i] = h.ff.IsBlacklisted(name, model)
	}
	return out
}

// ruleEnabled 返回 rule 每个上游的启用状态（nil 表示配置缺失，前端按未知处理）。
func (h *Handler) ruleEnabled(upstreams []string) []bool {
	if h.cfg == nil {
		return nil
	}
	out := make([]bool, len(upstreams))
	for i, name := range upstreams {
		out[i] = h.upstreamEnabled(name)
	}
	return out
}

// Rules 返回路由规则列表；rule 的 strategy 为空时继承默认策略，
// Rules 返回路由规则列表。优先从数据库读取（h.store 非 nil），否则从配置读取。
func (h *Handler) Rules(w http.ResponseWriter, _ *http.Request) {
	if h.store != nil {
		rows, err := h.store.ListRoutingRules()
		if err == nil {
			resp := make([]ruleResponse, 0, len(rows))
			for _, r := range rows {
				var upstreams []string
				json.Unmarshal([]byte(r.Upstreams), &upstreams)
				strategy := r.Strategy
				if strategy == "" {
					strategy = h.cfg.Routing.DefaultStrategy
				}
				ff := h.ruleFastFail(r.Model, upstreams)
				en := h.ruleEnabled(upstreams)
				hs := h.ruleHealthState(upstreams)
				upstreams, ff, en, hs = sortRuleUpstreams(upstreams, ff, en, hs, h)
				resp = append(resp, ruleResponse{
					Model:       r.Model,
					Strategy:    strategy,
					Upstreams:   upstreams,
					FastFail:    ff,
					Enabled:     en,
					HealthState: hs,
				})
			}
			writeJSON(w, resp)
			return
		}
	}
	// fallback: 从静态配置读取
	resp := make([]ruleResponse, 0, len(h.cfg.Routing.Rules))
	for _, rule := range h.cfg.Routing.Rules {
		strategy := rule.Strategy
		if strategy == "" {
			strategy = h.cfg.Routing.DefaultStrategy
		}
		ff := h.ruleFastFail(rule.Model, rule.Upstreams)
		en := h.ruleEnabled(rule.Upstreams)
		hs := h.ruleHealthState(rule.Upstreams)
		upstreams, ff, en, hs := sortRuleUpstreams(rule.Upstreams, ff, en, hs, h)
		resp = append(resp, ruleResponse{
			Model:       rule.Model,
			Strategy:    strategy,
			Upstreams:   upstreams,
			FastFail:    ff,
			Enabled:     en,
			HealthState: hs,
		})
	}
	writeJSON(w, resp)
}

// serverSummary 是 /admin/config 中 server 块的摘要（脱敏，无 api_key 明文）。
type serverSummary struct {
	Port              int  `json:"port"`
	APIKeysConfigured bool `json:"api_keys_configured"`
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

// endpoints 是网关对外暴露的端点静态清单。
var endpoints = []endpointInfo{
	{Path: "POST /v1/chat/completions", Desc: "OpenAI 对话（流式+非流式）"},
	{Path: "POST /v1/embeddings", Desc: "OpenAI 嵌入"},
	{Path: "POST /v1/images/generations", Desc: "文生图"},
	{Path: "POST /v1/audio/speech", Desc: "语音合成 TTS"},
	{Path: "POST /v1/audio/transcriptions", Desc: "语音转文字 STT"},
	{Path: "POST /v1/messages", Desc: "Claude(Anthropic) 协议"},
	{Path: "POST /api/chat", Desc: "Ollama 原生对话"},
	{Path: "POST /api/generate", Desc: "Ollama 原生生成"},
	{Path: "POST /api/embed", Desc: "Ollama 原生嵌入"},
}

// Config 返回配置摘要：端口、是否配置了 api_key（仅布尔值，绝不返回明文）
// 以及端点清单。
func (h *Handler) Config(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, configResponse{
		Server: serverSummary{
			Port:              h.cfg.Server.Port,
			APIKeysConfigured: h.cfg.Server.APIKeys != "",
		},
		DefaultStrategy: h.cfg.Routing.DefaultStrategy,
		Endpoints:       endpoints,
	})
}

// writeJSON 以 application/json 输出 v。
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
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

// MetricsSummary 返回全局统计（支持 ?range=today|3d|7d|30d|all）。
func (h *Handler) MetricsSummary(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, metricsSummaryResponse{})
		return
	}
	var resp metricsSummaryResponse
	var err error
	if since := metricsSince(r); since != "" {
		err = h.store.DB().QueryRow(`
			SELECT COUNT(*) as total,
			       COALESCE(SUM(CASE WHEN status < 400 THEN 1 ELSE 0 END), 0) as successes,
			       COALESCE(SUM(tokens), 0) as tokens,
			       COALESCE(AVG(duration_ms), 0) as avg_ms
			FROM request_log WHERE ts >= ?
		`, since).Scan(&resp.TotalRequests, &resp.TotalSuccesses, &resp.TotalTokens, &resp.AvgLatencyMS)
	} else {
		err = h.store.DB().QueryRow(`
			SELECT COUNT(*) as total,
			       COALESCE(SUM(CASE WHEN status < 400 THEN 1 ELSE 0 END), 0) as successes,
			       COALESCE(SUM(tokens), 0) as tokens,
			       COALESCE(AVG(duration_ms), 0) as avg_ms
			FROM request_log
		`).Scan(&resp.TotalRequests, &resp.TotalSuccesses, &resp.TotalTokens, &resp.AvgLatencyMS)
	}
	if err != nil {
		writeJSON(w, metricsSummaryResponse{})
		return
	}
	if resp.TotalRequests > 0 {
		resp.SuccessRate = float64(resp.TotalSuccesses) / float64(resp.TotalRequests)
	}
	for _, up := range h.cfg.Upstreams {
		if h.hc.Status(up.Name) == health.StateHealthy {
			resp.ActiveUpstreams++
		}
	}
	writeJSON(w, resp)
}

// upstreamMetrics 是 GET /admin/metrics/upstreams 的单个元素。
type upstreamMetrics struct {
	Name         string  `json:"name"`
	Requests     int64   `json:"requests"`
	Successes    int64   `json:"successes"`
	Failures     int64   `json:"failures"`
	SuccessRate  float64 `json:"success_rate"`
	AvgLatencyMS float64 `json:"avg_latency_ms"`
	TotalTokens  int64   `json:"total_tokens"`
	State        string  `json:"state"` // healthy / degraded / dead（来自健康检查）
	// 定时探测统计：健康度 = ProbeSuccess / (ProbeSuccess + ProbeFail)
	ProbeSuccess int64   `json:"probe_success"`
	ProbeFail    int64   `json:"probe_fail"`
	HealthRate   float64 `json:"health_rate"` // 探测成功率 0~1；无探测数据时 0
}

// MetricsUpstreams 返回每上游统计（支持 ?range=today|3d|7d|30d|all）。
func (h *Handler) MetricsUpstreams(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, []upstreamMetrics{})
		return
	}
	var rows *sql.Rows
	var err error
	if since := metricsSince(r); since != "" {
		rows, err = h.store.DB().Query(`
			SELECT upstream, COUNT(*) as requests,
			       COALESCE(SUM(CASE WHEN status < 400 THEN 1 ELSE 0 END), 0) as successes,
			       COUNT(*) - COALESCE(SUM(CASE WHEN status < 400 THEN 1 ELSE 0 END), 0) as failures,
			       COALESCE(AVG(duration_ms), 0) as avg_ms,
			       COALESCE(SUM(tokens), 0) as tokens
			FROM request_log WHERE ts >= ?
			GROUP BY upstream ORDER BY tokens DESC
		`, since)
	} else {
		rows, err = h.store.DB().Query(`
			SELECT upstream, COUNT(*) as requests,
			       COALESCE(SUM(CASE WHEN status < 400 THEN 1 ELSE 0 END), 0) as successes,
			       COUNT(*) - COALESCE(SUM(CASE WHEN status < 400 THEN 1 ELSE 0 END), 0) as failures,
			       COALESCE(AVG(duration_ms), 0) as avg_ms,
			       COALESCE(SUM(tokens), 0) as tokens
			FROM request_log
			GROUP BY upstream ORDER BY tokens DESC
		`)
	}
	if err != nil {
		writeJSON(w, []upstreamMetrics{})
		return
	}
	defer rows.Close()

	var out []upstreamMetrics
	for rows.Next() {
		var m upstreamMetrics
		if err := rows.Scan(&m.Name, &m.Requests, &m.Successes, &m.Failures, &m.AvgLatencyMS, &m.TotalTokens); err != nil {
			continue
		}
		if m.Requests > 0 {
			m.SuccessRate = float64(m.Successes) / float64(m.Requests)
		}
		// 补充健康状态与定时探测统计。
		// 探测统计优先从 DB 按 range 聚合（重启保留历史），无 store 数据时回退到内存计数。
		if h.hc != nil {
			switch h.hc.Status(m.Name) {
			case health.StateHealthy:
				m.State = "healthy"
			case health.StateDegraded:
				m.State = "degraded"
			case health.StateDead:
				m.State = "dead"
			default:
				m.State = "unknown"
			}
			// 从 health_probe_log 按 range 统计（与请求日志同一时间窗口）
			var ps, pf int64
			if since := metricsSince(r); since != "" {
				ps, pf = h.store.ProbeStats(m.Name, since)
			} else {
				ps, pf = h.store.ProbeStats(m.Name, "")
			}
			if ps+pf > 0 {
				m.ProbeSuccess, m.ProbeFail = ps, pf
				m.HealthRate = float64(ps) / float64(ps+pf)
			} else {
				// 无持久化记录（新表/未开启），回退到内存计数
				m.ProbeSuccess, m.ProbeFail = h.hc.ProbeStats(m.Name)
				m.HealthRate = h.hc.ProbeRate(m.Name)
			}
		}
		out = append(out, m)
	}
	writeJSON(w, out)
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
}

// MetricsByAPIKey 返回按下游 API Key 聚合的统计（支持 ?range=...）。
// 用于区分不同 AI 程序/客户端的使用量，看哪个 Key 用得多。
func (h *Handler) MetricsByAPIKey(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, []apiKeyMetrics{})
		return
	}
	since := metricsSince(r)
	rows := h.store.MetricsByAPIKey(since)
	out := make([]apiKeyMetrics, 0, len(rows))
	for _, row := range rows {
		m := apiKeyMetrics{
			Name:         row.Name,
			Requests:     row.Requests,
			Successes:    row.Successes,
			AvgLatencyMS: row.AvgLatencyMS,
			TotalTokens:  row.TotalTokens,
		}
		if m.Requests > 0 {
			m.SuccessRate = float64(m.Successes) / float64(m.Requests)
		}
		out = append(out, m)
	}
	writeJSON(w, out)
}

// APIKeyModelUsage 是单个 API Key 的模型使用分布。
type APIKeyModelUsage struct {
	Model string `json:"model"`
	Count int64  `json:"count"`
}

// MetricsByAPIKeyModels 返回指定 API Key 的模型使用分布（支持 ?range=...）。
func (h *Handler) MetricsByAPIKeyModels(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, []APIKeyModelUsage{})
		return
	}
	name := r.PathValue("name")
	if name == "" {
		writeJSON(w, []APIKeyModelUsage{})
		return
	}
	since := metricsSince(r)
	q := `SELECT model, COUNT(*) as count FROM request_log WHERE api_key = ?`
	var args []any = []any{name}
	if since != "" {
		q += ` AND ts >= ?`
		args = append(args, since)
	}
	q += ` GROUP BY model ORDER BY count DESC`
	rows, err := h.store.DB().Query(q, args...)
	if err != nil {
		writeJSON(w, []APIKeyModelUsage{})
		return
	}
	defer rows.Close()
	var out []APIKeyModelUsage
	for rows.Next() {
		var m APIKeyModelUsage
		if err := rows.Scan(&m.Model, &m.Count); err != nil {
			continue
		}
		out = append(out, m)
	}
	writeJSON(w, out)
}

// MetricsHourly 返回 24h 逐小时趋势。
func (h *Handler) MetricsHourly(w http.ResponseWriter, _ *http.Request) {
	if h.store == nil {
		writeJSON(w, []hourlyBucket{})
		return
	}
	rows, err := h.store.DB().Query(`
		SELECT strftime('%Y-%m-%dT%H:00:00Z', ts) as hour,
		       COUNT(*) as requests,
		       COALESCE(SUM(CASE WHEN status < 400 THEN 1 ELSE 0 END), 0) as successes,
		       COALESCE(SUM(tokens), 0) as tokens
		FROM request_log WHERE ts >= datetime('now', '-1 day')
		GROUP BY hour ORDER BY hour ASC
	`)
	if err != nil {
		writeJSON(w, []hourlyBucket{})
		return
	}
	defer rows.Close()

	var out []hourlyBucket
	for rows.Next() {
		var b hourlyBucket
		if err := rows.Scan(&b.Hour, &b.Requests, &b.Successes, &b.Tokens); err != nil {
			continue
		}
		out = append(out, b)
	}
	writeJSON(w, out)
}

// MetricsDaily 返回按天聚合的趋势（支持 ?range=today|3d|7d|30d|all）。
// 无请求的日期也会补零返回，保证折线图连续完整。
func (h *Handler) MetricsDaily(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, []dailyBucket{})
		return
	}
	since := metricsSince(r)
	loc := time.FixedZone("CST", 8*3600)
	now := time.Now().In(loc)

	// 查询按天聚合（ts 存 UTC，转本地日期分组）
	q := `SELECT strftime('%Y-%m-%d', ts, '+8 hours') as day,
	             COUNT(*) as requests,
	             COALESCE(SUM(CASE WHEN status < 400 THEN 1 ELSE 0 END), 0) as successes,
	             COALESCE(SUM(tokens), 0) as tokens
	      FROM request_log`
	var args []any
	if since != "" {
		q += ` WHERE ts >= ?`
		args = append(args, since)
	}
	q += ` GROUP BY day ORDER BY day ASC`
	rows, err := h.store.DB().Query(q, args...)
	if err != nil {
		writeJSON(w, []dailyBucket{})
		return
	}
	defer rows.Close()

	agg := map[string]dailyBucket{}
	for rows.Next() {
		var b dailyBucket
		if err := rows.Scan(&b.Date, &b.Requests, &b.Successes, &b.Tokens); err != nil {
			continue
		}
		agg[b.Date] = b
	}

	// 确定起始日期：since 对应那天（today 取今天）；all 取最早有数据的日期
	var start time.Time
	switch r.URL.Query().Get("range") {
	case "today":
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	case "all":
		if len(agg) == 0 {
			writeJSON(w, []dailyBucket{})
			return
		}
		first := ""
		for d := range agg {
			if first == "" || d < first {
				first = d
			}
		}
		t, _ := time.ParseInLocation("2006-01-02", first, loc)
		start = t
	default:
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
		if since != "" {
			if t, err := time.Parse(time.RFC3339, since); err == nil {
				start = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
			}
		}
	}

	// 从 start 到 now 逐天补零
	var out []dailyBucket
	for d := start; !d.After(now); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		if b, ok := agg[key]; ok {
			out = append(out, b)
		} else {
			out = append(out, dailyBucket{Date: key})
		}
	}
	writeJSON(w, out)
}
func (h *Handler) GetRetryConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]interface{}{
		"max_retries":       h.cfg.Retry.MaxRetries,
		"retry_statuses":    h.cfg.Retry.RetryStatuses,
		"retry_keywords":    h.cfg.Retry.RetryKeywords,
		"fast_fail_minutes": h.cfg.Retry.FastFailMinutes,
	})
}

// GetAllConfig 返回配置表中所有 key-value 对（GET /admin/config）。
func (h *Handler) GetAllConfig(w http.ResponseWriter, _ *http.Request) {
	if h.store == nil {
		writeJSON(w, map[string]interface{}{"error": "store not available"})
		return
	}
	all, err := h.store.GetAllConfig()
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	writeJSON(w, all)
}

// UpdateConfig 更新配置项（PUT /admin/config），更新后自动热重载。
func (h *Handler) UpdateConfig(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, map[string]string{"error": "store not available"})
		return
	}
	var req map[string]string
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "invalid request"})
		return
	}
	// 只读保护：proxy.cooldown_* 属于运行期内部机制，不允许通过配置修改。
	// 前端已置灰，这里做后端兜底拦截，防止 API 直调绕过。
	// 注意：2026-08-06 用户要求改为可编辑，此拦截已移除。
	for k, v := range req {
		if err := h.store.SetConfig(k, v); err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
	}
	if h.reload != nil {
		h.reload()
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// ===== Upstream CRUD =====

// CreateUpstream 添加上游（POST /admin/upstreams）。
func (h *Handler) CreateUpstream(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	var req store.UpstreamRow
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	// enabled 显式传入时按传入值，未传默认启用（CreateUpstream 内部处理）
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) == nil {
		if v, ok := raw["enabled"]; ok {
			var e int
			if json.Unmarshal(v, &e) == nil {
				req.EnabledPtr = &e
			}
		}
	}
	if err := h.store.CreateUpstream(&req); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if h.reload != nil {
		h.reload()
	}
	writeJSON(w, req)
}

// UpdateUpstream 更新上游（PUT /admin/upstreams/{name}）。
func (h *Handler) UpdateUpstream(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	var req store.UpstreamRow
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	// 名称保护：body 未带 name 时用路径里的 name，避免被空串清空
	if strings.TrimSpace(req.Name) == "" {
		req.Name = name
	}
	// enabled 字段显式传入（含 false/0）时才允许改启停状态；
	// 未传时保持原值，避免编辑表单漏传导致上游被误禁用。
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) == nil {
		if v, ok := raw["enabled"]; ok {
			var e int
			if json.Unmarshal(v, &e) == nil {
				req.EnabledPtr = &e
			}
		}
	}
	if err := h.store.UpdateUpstream(name, &req); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if h.reload != nil {
		h.reload()
	}
	writeJSON(w, req)
}

// ToggleUpstream 切换上游启用/禁用（PUT /admin/upstreams/{name}/toggle）。
// 禁用的上游不参与转发路由（selectCandidates 过滤），立即生效（触发 reload）。
func (h *Handler) ToggleUpstream(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if h.store == nil {
		writeJSON(w, map[string]string{"error": "store not available"})
		return
	}
	up, err := h.store.GetUpstream(name)
	if err != nil {
		writeJSON(w, map[string]string{"error": "upstream not found"})
		return
	}
	enabled := 1
	if up.Enabled == 1 {
		enabled = 0
	}
	if err := h.store.SetUpstreamEnabled(name, enabled); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if h.reload != nil {
		if err := h.reload(); err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
	}
	writeJSON(w, map[string]any{"status": "ok", "enabled": enabled == 1})
}

// DeleteUpstream 删除上游（DELETE /admin/upstreams/{name}）。
func (h *Handler) DeleteUpstream(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := h.store.DeleteUpstream(name); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if h.reload != nil {
		h.reload()
	}
	writeJSON(w, map[string]string{"status": "deleted"})
}

// ruleHealthState 返回 rule 每个上游的健康检查状态（nil 表示未启用健康检查）。
func (h *Handler) ruleHealthState(upstreams []string) []string {
	if h.hc == nil {
		return nil
	}
	out := make([]string, len(upstreams))
	for i, name := range upstreams {
		out[i] = string(h.hc.Status(name))
	}
	return out
}

// sortUpstreamsJSON 解析规则上游列表（JSON 数组字符串），按路由优先级排序后重新序列化。
// 排序规则与 selectCandidates 一致：tier 升序（free→subscription→payg）→ 同 tier 内 weight 降序。
// 解析失败时原样返回，不阻断保存。
func (h *Handler) sortUpstreamsJSON(s string) string {
	var ups []string
	if err := json.Unmarshal([]byte(s), &ups); err != nil {
		return s
	}
	sorted, _, _, _ := sortRuleUpstreams(ups, nil, nil, nil, h)
	out, err := json.Marshal(sorted)
	if err != nil {
		return s
	}
	return string(out)
}

// ===== Routing Rule CRUD =====

// CreateRoutingRule 添加路由规则（POST /admin/rules）。
func (h *Handler) CreateRoutingRule(w http.ResponseWriter, r *http.Request) {
	var req store.RoutingRuleRow
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	req.Upstreams = h.sortUpstreamsJSON(req.Upstreams)
	if err := h.store.CreateRoutingRule(&req); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if h.reload != nil {
		h.reload()
	}
	writeJSON(w, req)
}

// UpdateRoutingRule 更新路由规则（PUT /admin/rules/{model}）。
func (h *Handler) UpdateRoutingRule(w http.ResponseWriter, r *http.Request) {
	model := r.PathValue("model")
	var req store.RoutingRuleRow
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	req.Upstreams = h.sortUpstreamsJSON(req.Upstreams)
	if err := h.store.UpdateRoutingRule(model, &req); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if h.reload != nil {
		h.reload()
	}
	writeJSON(w, req)
}

// DeleteRoutingRule 删除路由规则（DELETE /admin/rules/{model}）。
func (h *Handler) DeleteRoutingRule(w http.ResponseWriter, r *http.Request) {
	model := r.PathValue("model")
	if err := h.store.DeleteRoutingRule(model); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if h.reload != nil {
		h.reload()
	}
	writeJSON(w, map[string]string{"status": "deleted"})
}

// ===== EffortConfig（最佳思考等级）CRUD =====

// EffortConfigs 列出最佳思考等级配置（GET /admin/efforts）。
func (h *Handler) EffortConfigs(w http.ResponseWriter, _ *http.Request) {
	if h.store == nil {
		writeJSON(w, map[string]string{"error": "store not available"})
		return
	}
	all, err := h.store.ListEffortConfig()
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"items": all})
}

// CreateEffortConfig 添加最佳思考等级配置（POST /admin/efforts）。
func (h *Handler) CreateEffortConfig(w http.ResponseWriter, r *http.Request) {
	var req store.EffortConfigRow
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	if req.Model == "" {
		writeJSON(w, map[string]string{"error": "model is required"})
		return
	}
	if err := h.store.CreateEffortConfig(&req); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if h.reload != nil {
		h.reload()
	}
	writeJSON(w, req)
}

// UpdateEffortConfig 更新最佳思考等级配置（PUT /admin/efforts/{model}）。
func (h *Handler) UpdateEffortConfig(w http.ResponseWriter, r *http.Request) {
	model := r.PathValue("model")
	var req store.EffortConfigRow
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	if err := h.store.UpdateEffortConfig(model, &req); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if h.reload != nil {
		h.reload()
	}
	writeJSON(w, req)
}

// DeleteEffortConfig 删除最佳思考等级配置（DELETE /admin/efforts/{model}）。
func (h *Handler) DeleteEffortConfig(w http.ResponseWriter, r *http.Request) {
	model := r.PathValue("model")
	if err := h.store.DeleteEffortConfig(model); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if h.reload != nil {
		h.reload()
	}
	writeJSON(w, map[string]string{"status": "deleted"})
}

// Reload 从 DB 重新加载配置并重建路由/健康检查（POST /admin/reload）。
func (h *Handler) Reload(w http.ResponseWriter, _ *http.Request) {
	if h.reload == nil {
		writeJSON(w, map[string]string{"error": "reload not available"})
		return
	}
	if err := h.reload(); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]string{"status": "ok", "message": "config reloaded"})
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

// logProfile 是 /admin/logs 响应中 profile_map 的元素（client_addr → 已识别程序）。
type logProfile struct {
	Program    string  `json:"program"`    // 识别出的程序名（非空且非'未知'）
	Confidence float64 `json:"confidence"` // 置信度 0-1
}

// clientProfileMap 查询 client_profiles 中已识别出程序（program 非空且非'未知'）
// 的档案，返回 client_addr → {program, confidence} 映射，供请求日志页直接把
// 客户端列显示为程序名。store 为 nil 或查询失败时返回空 map（不阻断日志返回）。
func (h *Handler) clientProfileMap() map[string]logProfile {
	out := map[string]logProfile{}
	if h.store == nil {
		return out
	}
	rows, err := h.store.DB().Query(`SELECT client_addr, program, confidence
		FROM client_profiles WHERE program != '' AND program != '未知'`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var addr, program string
		var conf float64
		if rows.Scan(&addr, &program, &conf) == nil {
			out[addr] = logProfile{Program: program, Confidence: conf}
		}
	}
	return out
}

// RequestLogs 返回最近请求日志（GET /admin/logs?limit=50&offset=0&upstream=xx&model=yy）。
// 支持按上游/模型筛选与分页；响应含 total、筛选选项和 profile_map
// （client_addr → 已识别程序名，供前端客户端列显示程序名）。
func (h *Handler) RequestLogs(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, map[string]interface{}{"total": 0, "limit": 50, "offset": 0, "logs": []map[string]interface{}{}, "profile_map": map[string]logProfile{}})
		return
	}
	limit := 50
	offset := 0
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	if o := r.URL.Query().Get("offset"); o != "" {
		if n, err := strconv.Atoi(o); err == nil && n >= 0 {
			offset = n
		}
	}
	// 筛选条件（值走参数绑定，不拼 SQL）
	where := ""
	args := []interface{}{}
	if up := r.URL.Query().Get("upstream"); up != "" {
		where += " AND upstream = ?"
		args = append(args, up)
	}
	if m := r.URL.Query().Get("model"); m != "" {
		where += " AND model = ?"
		args = append(args, m)
	}
	if where != "" {
		where = " WHERE " + strings.TrimPrefix(where, " AND ")
	}

	// 总数（分页用）
	var total int64
	countArgs := append([]interface{}{}, args...)
	if err := h.store.DB().QueryRow("SELECT COUNT(*) FROM request_log"+where, countArgs...).Scan(&total); err != nil {
		total = 0
	}

	// 分页查询（按 id DESC 而非 ts DESC——ts 转为东八区字符串后字典序会乱）
	queryArgs := append([]interface{}{}, args...)
	queryArgs = append(queryArgs, limit, offset)
	rows, err := h.store.DB().Query(`
				SELECT ts, upstream, model, endpoint, status, duration_ms, prompt_tokens, completion_tokens, tokens, prompt_cache_hit_tokens, prompt_cache_miss_tokens, api_key, cost, upstream_model, client_addr, user_agent
				FROM request_log`+where+` ORDER BY id DESC LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		writeJSON(w, map[string]interface{}{"total": total, "limit": limit, "offset": offset, "logs": []map[string]interface{}{}, "profile_map": map[string]logProfile{}})
		return
	}
	defer rows.Close()

	var out []map[string]interface{}
	for rows.Next() {
		var ts, upstream, model, endpoint, apiKey, upstreamModel, clientAddr, userAgent string
		var status, durationMs, promptTokens, completionTokens, tokens, cacheHitTokens, cacheMissTokens int64
		var cost float64
		if err := rows.Scan(&ts, &upstream, &model, &endpoint, &status, &durationMs, &promptTokens, &completionTokens, &tokens, &cacheHitTokens, &cacheMissTokens, &apiKey, &cost, &upstreamModel, &clientAddr, &userAgent); err != nil {
			continue
		}
		out = append(out, map[string]interface{}{
			"ts":                       fmtCST(ts),
			"upstream":                 upstream,
			"model":                    model,
			"endpoint":                 endpoint,
			"status":                   status,
			"duration_ms":              durationMs,
			"prompt_tokens":            promptTokens,
			"completion_tokens":        completionTokens,
			"tokens":                   tokens,
			"prompt_cache_hit_tokens":  cacheHitTokens,
			"prompt_cache_miss_tokens": cacheMissTokens,
			"api_key":                  apiKey,
			"cost":                     cost,
			"upstream_model":           upstreamModel,
			"client_addr":              clientAddr,
			"user_agent":               userAgent,
		})
	}

	// 筛选选项：日志中出现过的上游与模型（distinct）
	upstreams := []string{}
	models := []string{}
	if rows2, err := h.store.DB().Query("SELECT DISTINCT upstream FROM request_log ORDER BY upstream"); err == nil {
		for rows2.Next() {
			var v string
			if rows2.Scan(&v) == nil && v != "" {
				upstreams = append(upstreams, v)
			}
		}
		rows2.Close()
	}
	if rows3, err := h.store.DB().Query("SELECT DISTINCT model FROM request_log ORDER BY model"); err == nil {
		for rows3.Next() {
			var v string
			if rows3.Scan(&v) == nil && v != "" {
				models = append(models, v)
			}
		}
		rows3.Close()
	}

	writeJSON(w, map[string]interface{}{
		"total":       total,
		"limit":       limit,
		"offset":      offset,
		"logs":        out,
		"filters":     map[string]interface{}{"upstreams": upstreams, "models": models},
		"profile_map": h.clientProfileMap(),
	})
}

// RecalcCost 重算历史请求费用（POST /admin/logs/recalc-cost）。
// 对缓存字段全 0 且有输入 token 的历史请求，按「未命中价全额」口径重算 cost
// （修复 calcCost 之前无缓存统计时输入白嫖的问题）。返回更新条数。
func (h *Handler) RecalcCost(w http.ResponseWriter, _ *http.Request) {
	if h.store == nil {
		writeJSON(w, map[string]string{"error": "store not available"})
		return
	}
	updated, err := h.store.RecalcCost()
	if err != nil {
		writeJSON(w, map[string]interface{}{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]interface{}{"updated": updated})
}

// CloneUpstream 克隆上游：读取原上游配置，改名称和 API key 后创建（POST /admin/upstreams/{name}/clone）。
func (h *Handler) CloneUpstream(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if h.store == nil {
		writeJSON(w, map[string]string{"error": "store not available"})
		return
	}
	orig, err := h.store.GetUpstream(name)
	if err != nil {
		writeJSON(w, map[string]string{"error": "upstream not found: " + err.Error()})
		return
	}
	var req struct {
		Name   string `json:"name"`
		APIKey string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	if req.Name == "" {
		writeJSON(w, map[string]string{"error": "new name is required"})
		return
	}
	clone := *orig
	clone.Name = req.Name
	if req.APIKey != "" {
		clone.APIKey = req.APIKey
	}
	// 保留原上游的启用状态（克隆语义）
	clone.EnabledPtr = &clone.Enabled
	if err := h.store.CreateUpstream(&clone); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if h.reload != nil {
		h.reload()
	}
	writeJSON(w, clone)
}

// Models 返回 OpenAI 兼容的模型列表（GET /v1/models）。
func (h *Handler) Models(w http.ResponseWriter, _ *http.Request) {
	type modelObj struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	var models []modelObj
	seen := make(map[string]bool)
	for _, rule := range h.cfg.Routing.Rules {
		if !seen[rule.Model] {
			seen[rule.Model] = true
			models = append(models, modelObj{
				ID:      rule.Model,
				Object:  "model",
				Created: time.Now().Unix(),
				OwnedBy: "xuanji",
			})
		}
	}
	writeJSON(w, map[string]interface{}{
		"object": "list",
		"data":   models,
	})
}

// APIKeys 列出所有下游 API Key（GET /admin/api-keys）。
// 数据来自 api_tokens 表；store 为 nil 时返回空列表。
func (h *Handler) APIKeys(w http.ResponseWriter, _ *http.Request) {
	if h.store == nil {
		writeJSON(w, []store.APIToken{})
		return
	}
	tokens, err := h.store.ListAPITokens()
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, tokens)
}

// AddAPIKey 创建下游 API Key（POST /admin/api-keys）。
// 请求体：{"name":"用途","key":"可选自定义key","remark":"备注"}。
// key 为空时自动生成 sk-xxx 格式。
func (h *Handler) AddAPIKey(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, map[string]string{"error": "store not available"})
		return
	}
	var req struct {
		Name   string `json:"name"`
		Key    string `json:"key"`
		Remark string `json:"remark"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	key := strings.TrimSpace(req.Key)
	if key == "" {
		key = "sk-" + generateToken()
	}
	if h.store.APITokenExists(key) {
		writeJSON(w, map[string]string{"error": "key already exists"})
		return
	}
	tok, err := h.store.CreateAPIToken(req.Name, key, req.Remark)
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	h.refreshAuth()
	writeJSON(w, map[string]interface{}{"status": "ok", "id": tok.ID, "key": tok.Key})
}

// DeleteAPIKey 删除下游 API Key（DELETE /admin/api-keys/{id}）。
// 路径参数为数字 id（不再用 key 字符串，因为 key 可能含特殊字符）。
func (h *Handler) DeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, map[string]string{"error": "store not available"})
		return
	}
	idStr := r.PathValue("key")
	id, err := strconv.ParseUint(idStr, 10, 32)
	if err != nil {
		writeJSON(w, map[string]string{"error": "invalid id"})
		return
	}
	if err := h.store.DeleteAPIToken(uint(id)); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	h.refreshAuth()
	writeJSON(w, map[string]string{"status": "ok"})
}

// SetAPIKeyEnabled 启用/禁用下游 API Key（PUT /admin/api-keys/{id}/toggle）。
func (h *Handler) SetAPIKeyEnabled(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, map[string]string{"error": "store not available"})
		return
	}
	idStr := r.PathValue("id")
	id, err := strconv.ParseUint(idStr, 10, 32)
	if err != nil {
		writeJSON(w, map[string]string{"error": "invalid id"})
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := h.store.SetAPITokenEnabled(uint(id), req.Enabled); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	h.refreshAuth()
	writeJSON(w, map[string]string{"status": "ok"})
}

// Login 管理端用户名密码登录（POST /admin/login）。
// 验证 bcrypt 密码后签发 JWT，有效期 24h。
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	if req.Username == "" || req.Password == "" {
		writeJSON(w, map[string]string{"error": "username and password required"})
		return
	}
	if h.store == nil {
		writeJSON(w, map[string]string{"error": "store not available"})
		return
	}
	user, err := h.store.GetUser(req.Username)
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if user == nil || bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)) != nil {
		writeJSON(w, map[string]string{"error": "invalid username or password"})
		return
	}
	secret := h.JWTSecret()
	token := auth.SignToken(secret, user.Username, 24*time.Hour)
	writeJSON(w, map[string]string{"status": "ok", "token": token, "username": user.Username})
}

// ChangePassword 修改当前用户密码（PUT /admin/password）。
// 需携带旧密码 + 新密码，验证通过后更新。
func (h *Handler) ChangePassword(w http.ResponseWriter, r *http.Request) {
	username, _ := r.Context().Value("xuanji-username").(string)
	if username == "" {
		writeJSON(w, map[string]string{"error": "unauthorized"})
		return
	}
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	if req.NewPassword == "" || len(req.NewPassword) < 6 {
		writeJSON(w, map[string]string{"error": "新密码至少 6 位"})
		return
	}
	if h.store == nil {
		writeJSON(w, map[string]string{"error": "store not available"})
		return
	}
	user, err := h.store.GetUser(username)
	if err != nil || user == nil {
		writeJSON(w, map[string]string{"error": "user not found"})
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.OldPassword)) != nil {
		writeJSON(w, map[string]string{"error": "旧密码错误"})
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if err := h.store.UpdateUserPassword(username, string(hash)); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// JWTSecret 读取或生成 JWT 签名密钥（config 表 admin.jwt_secret）。
func (h *Handler) JWTSecret() string {
	if h.store == nil {
		return "xuanji-default-secret"
	}
	v, _ := h.store.GetConfig("admin.jwt_secret")
	if v != "" {
		return v
	}
	v = "xuanji-" + generateToken()
	_ = h.store.SetConfig("admin.jwt_secret", v)
	return v
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
// （数据库是配置唯一来源，内存 cfg 可能因直改 DB 未 reload 而过期）。
func (h *Handler) upstreamByName(name string) *config.Upstream {
	if h.store != nil {
		rows, err := h.store.ListUpstreams()
		if err == nil {
			for i := range rows {
				u := &rows[i]
				if u.Name == name {
					return &config.Upstream{
						Name:         u.Name,
						Type:         u.Type,
						BaseURL:      u.BaseURL,
						APIKey:       u.APIKey,
						Tier:         u.Tier,
						Priority:     u.Priority,
						Weight:       u.Weight,
						Models:       parseStringSlice(u.Models),
						ModelMapping: parseStringMap(u.ModelMapping),
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

// UpstreamModels 从上游拉取全部模型（GET /admin/upstreams/{name}/models）。
// 用该上游自己的 api_key 调用 {base_url}/models，返回原始模型名列表。
func (h *Handler) UpstreamModels(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	up := h.upstreamByName(name)
	if up == nil {
		writeJSON(w, map[string]string{"error": "upstream not found"})
		return
	}

	base := strings.TrimSuffix(up.BaseURL, "/")
	target := base + "/models"

	httpReq, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if up.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+up.APIKey)
	}

	client := &http.Client{Timeout: upstreamTestTimeout(h.cfg)}
	resp, err := client.Do(httpReq)
	if err != nil {
		writeJSON(w, map[string]string{"error": "请求失败: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		writeJSON(w, map[string]any{"error": fmt.Sprintf("上游返回 HTTP %d: %s", resp.StatusCode, truncateStr(string(respBody), 200))})
		return
	}
	var data struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &data); err != nil {
		writeJSON(w, map[string]string{"error": "解析响应失败: " + err.Error()})
		return
	}
	var models []string
	for _, m := range data.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	writeJSON(w, map[string]any{"status": "ok", "models": models, "count": len(models)})
}

// truncateStr 截断长字符串。
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Discounts 列出所有渠道优惠时段（GET /admin/discounts）。
func (h *Handler) Discounts(w http.ResponseWriter, _ *http.Request) {
	if h.store == nil {
		writeJSON(w, []store.Discount{})
		return
	}
	list, err := h.store.ListDiscounts()
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, list)
}

// AddDiscount 创建优惠时段（POST /admin/discounts）。
func (h *Handler) AddDiscount(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, map[string]string{"error": "store not available"})
		return
	}
	var d store.Discount
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		writeJSON(w, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	if d.Upstream == "" {
		writeJSON(w, map[string]string{"error": "上游名称必填"})
		return
	}
	if d.ModelPattern == "" {
		d.ModelPattern = "*"
	}
	if d.Discount <= 0 || d.Discount > 1 {
		writeJSON(w, map[string]string{"error": "折扣率需在 (0,1] 之间，如 0.5=半价"})
		return
	}
	if err := h.store.CreateDiscount(&d); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, d)
}

// UpdateDiscount 更新优惠时段（PUT /admin/discounts/{id}）。
func (h *Handler) UpdateDiscount(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, map[string]string{"error": "store not available"})
		return
	}
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 32)
	if err != nil {
		writeJSON(w, map[string]string{"error": "invalid id"})
		return
	}
	var d store.Discount
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		writeJSON(w, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	if d.Discount <= 0 || d.Discount > 1 {
		writeJSON(w, map[string]string{"error": "折扣率需在 (0,1] 之间"})
		return
	}
	if err := h.store.UpdateDiscount(uint(id), &d); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// DeleteDiscount 删除优惠时段（DELETE /admin/discounts/{id}）。
func (h *Handler) DeleteDiscount(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, map[string]string{"error": "store not available"})
		return
	}
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 32)
	if err != nil {
		writeJSON(w, map[string]string{"error": "invalid id"})
		return
	}
	if err := h.store.DeleteDiscount(uint(id)); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// Prices 列出所有模型单价（GET /admin/prices）。
func (h *Handler) Prices(w http.ResponseWriter, _ *http.Request) {
	if h.store == nil {
		writeJSON(w, []store.ModelPrice{})
		return
	}
	writeJSON(w, h.store.ListPrices())
}

// AddPrice 新增/更新模型单价（POST /admin/prices，按 model upsert）。
func (h *Handler) AddPrice(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, map[string]string{"error": "store not available"})
		return
	}
	var p store.ModelPrice
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeJSON(w, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	p.Model = strings.TrimSpace(p.Model)
	if p.Model == "" {
		writeJSON(w, map[string]string{"error": "模型名必填（* 表示默认价）"})
		return
	}
	if p.PriceInput < 0 || p.PriceCache < 0 || p.PriceOut < 0 {
		writeJSON(w, map[string]string{"error": "价格不能为负数"})
		return
	}
	if err := h.store.UpsertPrice(p); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, p)
}

// UpdatePrice 更新模型单价（PUT /admin/prices/{model}）。
func (h *Handler) UpdatePrice(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, map[string]string{"error": "store not available"})
		return
	}
	model := r.PathValue("model")
	if model == "" {
		writeJSON(w, map[string]string{"error": "model required"})
		return
	}
	var p store.ModelPrice
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeJSON(w, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	if p.PriceInput < 0 || p.PriceCache < 0 || p.PriceOut < 0 {
		writeJSON(w, map[string]string{"error": "价格不能为负数"})
		return
	}
	p.Model = model
	if err := h.store.UpsertPrice(p); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, p)
}

// DeletePrice 删除模型单价（DELETE /admin/prices/{model}）。默认价（*）不可删。
func (h *Handler) DeletePrice(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, map[string]string{"error": "store not available"})
		return
	}
	model := r.PathValue("model")
	if model == "*" {
		writeJSON(w, map[string]string{"error": "默认价不可删除"})
		return
	}
	if err := h.store.DeletePrice(model); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// costMetricsResponse 是 GET /admin/metrics/cost 的响应。
type costMetricsResponse struct {
	TotalCost  float64         `json:"total_cost"` // 时间段内总费用（元）
	ByUpstream []store.CostRow `json:"by_upstream"`
	ByAPIKey   []store.CostRow `json:"by_api_key"`
	ByModel    []store.CostRow `json:"by_model"`
}

// MetricsCost 返回费用统计（GET /admin/metrics/cost，支持 ?range=today|3d|7d|30d|all）。
func (h *Handler) MetricsCost(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, costMetricsResponse{})
		return
	}
	since := metricsSince(r)
	until := ""
	if since != "" {
		until = time.Now().UTC().Format(time.RFC3339)
	}
	var resp costMetricsResponse
	resp.TotalCost, _ = h.store.TotalCost(since, until)
	resp.ByUpstream = h.store.CostByUpstream(since, until)
	resp.ByAPIKey = h.store.CostByAPIKey(since, until)
	resp.ByModel = h.store.CostByModel(since, until)
	writeJSON(w, resp)
}

// TestUpstream 直接使用上游自己的 API Key 测试（POST /admin/upstreams/{name}/test）。
// 绕过网关路由，直连该上游的 /v1/chat/completions，验证 key 与模型可用性。
func (h *Handler) TestUpstream(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	up := h.upstreamByName(name)
	if up == nil {
		writeJSON(w, map[string]string{"error": "upstream not found"})
		return
	}

	var req struct {
		Model     string `json:"model"`
		Content   string `json:"content"`
		MaxTokens int    `json:"max_tokens"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Model == "" {
		req.Model = "deepseek-v4-flash"
	}
	if req.Content == "" {
		req.Content = "你好，请用一句话回复"
	}
	if req.MaxTokens <= 0 {
		req.MaxTokens = 512
	}

	// 应用 model_mapping：把客户端简单名还原为上游真实模型名
	realModel := req.Model
	if len(up.ModelMapping) > 0 {
		if v, ok := up.ModelMapping[req.Model]; ok && v != "" {
			realModel = v
		}
	}

	// 构造直连请求体（用还原后的真实模型名）
	body := map[string]any{
		"model":      realModel,
		"messages":   []map[string]string{{"role": "user", "content": req.Content}},
		"max_tokens": req.MaxTokens,
	}
	payload, _ := json.Marshal(body)

	base := strings.TrimSuffix(up.BaseURL, "/")
	target := base + "/chat/completions"

	httpReq, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if up.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+up.APIKey)
	}

	client := &http.Client{Timeout: upstreamTestTimeout(h.cfg)}
	resp, err := client.Do(httpReq)
	if err != nil {
		writeJSON(w, map[string]string{"error": "请求失败: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		writeJSON(w, map[string]any{
			"status": "fail",
			"code":   resp.StatusCode,
			"error":  string(respBody),
		})
		return
	}
	// 200 但内容是空完成（思考型模型 max_tokens 不足被截断）：HTTP 正常不代表响应有效
	warning := ""
	if proxy.IsEmptyCompletion(respBody) {
		warning = "⚠ 响应内容为空：疑似思考型模型（商汤日日新等默认开启思考）max_tokens 不足，思考未完成即被截断（finish_reason=length）。请调大 max_tokens（如 512+）或检查模型思考模式。"
	}
	writeJSON(w, map[string]any{
		"status":  "ok",
		"body":    json.RawMessage(respBody),
		"warning": warning,
	})
}
