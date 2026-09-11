package admin

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/icefairy/xuanji/internal/health"
)

// writeCached 写 JSON 响应，并缓存到 metricsCache（TTL 内后续请求直接命中）。
func (h *Handler) writeCached(w http.ResponseWriter, r *http.Request, payload any) {
	key := r.URL.Path + "?" + r.URL.RawQuery
	if b, ok := h.cacheGet(key); ok {
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
		return
	}
	b, err := json.Marshal(payload)
	if err != nil {
		writeJSON(w, payload)
		return
	}
	h.cacheSet(key, b)
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func (h *Handler) cacheGet(key string) ([]byte, bool) {
	h.metricsCacheMu.RLock()
	e, ok := h.metricsCache[key]
	h.metricsCacheMu.RUnlock()
	if ok && time.Now().Before(e.exp) {
		return e.data, true
	}
	return nil, false
}

func (h *Handler) cacheSet(key string, data []byte) {
	h.metricsCacheMu.Lock()
	h.metricsCache[key] = cacheEntry{data: data, exp: time.Now().Add(h.metricsCacheTTL)}
	h.metricsCacheMu.Unlock()
}

// clearMetricsCache 清空统计缓存（重算完成后调用，避免旧数据滞留）。
func (h *Handler) clearMetricsCache() {
	h.metricsCacheMu.Lock()
	h.metricsCache = map[string]cacheEntry{}
	h.metricsCacheMu.Unlock()
}

// MetricsRebuild 全量重算历史每日预聚合（daily_stats）。立即返回 queued，后台异步执行，
// 重算完成后清空统计缓存。用于纠正历史统计不一致或首次填充预聚合表。
func (h *Handler) MetricsRebuild(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, map[string]any{"status": "skipped", "reason": "store not available"})
		return
	}
	go func() {
		if err := h.store.RebuildDailyStats(); err != nil {
			return
		}
		h.clearMetricsCache()
	}()
	writeJSON(w, map[string]any{"status": "queued"})
}

// MetricsSummary 返回全局统计（支持 ?range=today|3d|7d|30d|all）。
// MetricsSummary 返回全局统计（支持 ?range=today|3d|7d|30d|all）。
// 历史天走 daily_stats 预聚合，今天实时聚合，结果缓存 60s。
func (h *Handler) MetricsSummary(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		h.writeCached(w, r, metricsSummaryResponse{})
		return
	}
	loc := time.FixedZone("CST", 8*3600)
	agg, err := h.store.AggregateRange(metricsRangeStr(r), loc)
	if err != nil {
		h.writeCached(w, r, metricsSummaryResponse{})
		return
	}
	resp := metricsSummaryResponse{
		TotalRequests:  agg.Requests,
		TotalSuccesses: agg.Successes,
		TotalTokens:    agg.Tokens,
	}
	if agg.Requests > 0 {
		resp.SuccessRate = float64(agg.Successes) / float64(agg.Requests)
		resp.AvgLatencyMS = float64(agg.SumDurationMS) / float64(agg.Requests)
	}
	for _, up := range h.cfg.Upstreams {
		if h.hc != nil && h.hc.Status(up.Name) == health.StateHealthy {
			resp.ActiveUpstreams++
		}
	}
	h.writeCached(w, r, resp)
}

// MetricsUpstreams 返回每上游统计（支持 ?range=today|3d|7d|30d|all）。
// MetricsUpstreams 返回每上游统计（支持 ?range=today|3d|7d|30d|all）。
// 历史天走 daily_stats 预聚合，今天实时聚合，结果缓存 60s。
func (h *Handler) MetricsUpstreams(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		h.writeCached(w, r, []upstreamMetrics{})
		return
	}
	rangeStr := metricsRangeStr(r)
	loc := time.FixedZone("CST", 8*3600)
	agg, err := h.store.AggregateRange(rangeStr, loc)
	if err != nil {
		h.writeCached(w, r, []upstreamMetrics{})
		return
	}
	since := metricsSince(r)
	// 过滤已在上游管理中删除的上游（DB 现存 ∪ 内存配置并集视为"仍存在"），
	// 避免统计页展示僵尸上游。
	existing := h.existingUpstreamSet()
	// 一次性取所有上游探针统计（避免对每个上游串行扫百万行探针表）
	probeAll := h.store.ProbeStatsAll(since)
	out := make([]upstreamMetrics, 0, len(agg.ByUpstream))
	for name, d := range agg.ByUpstream {
		if !existing[name] {
			continue
		}
		m := upstreamMetrics{
			Name:           name,
			Requests:       d.Requests,
			Successes:      d.Successes,
			TotalTokens:    d.Tokens,
			ThinkingTokens: d.ThinkingTokens,
		}
		m.Failures = d.Requests - d.Successes
		if d.Requests > 0 {
			m.SuccessRate = float64(d.Successes) / float64(d.Requests)
			m.AvgLatencyMS = float64(d.SumDurationMS) / float64(d.Requests)
			// TTFT：只统计有 TTFT 数据的请求（流式请求）
			if d.SumTTFTMS > 0 {
				m.AvgTTFTMS = float64(d.SumTTFTMS) / float64(d.Requests)
			}
			// Tokens/秒：(completion_tokens + thinking_tokens) / (duration_ms / 1000)
			// 写入时已归一化：OpenAI 标准风格（agnes/o1）completion 已剥离思考，
			// 两种风格下 completion + thinking 均等于实际生成量，不重复计数。
			if d.SumDurationMS > 0 && (d.CompletionTokens+d.ThinkingTokens) > 0 {
				m.TokensPerSec = float64(d.CompletionTokens+d.ThinkingTokens) / (float64(d.SumDurationMS) / 1000.0)
			}
		}
		if h.hc != nil {
			switch h.hc.Status(name) {
			case health.StateHealthy:
				m.State = "healthy"
			case health.StateDegraded:
				m.State = "degraded"
			case health.StateDead:
				m.State = "dead"
			default:
				m.State = "unknown"
			}
			var ps, pf int64
			if v, ok := probeAll[name]; ok && v[0]+v[1] > 0 {
				ps, pf = v[0], v[1]
				m.ProbeSuccess, m.ProbeFail = ps, pf
				m.HealthRate = float64(ps) / float64(ps+pf)
			} else {
				m.ProbeSuccess, m.ProbeFail = h.hc.ProbeStats(name)
				m.HealthRate = h.hc.ProbeRate(name)
			}
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TotalTokens > out[j].TotalTokens })
	h.writeCached(w, r, out)
}

// existingUpstreamSet 返回当前"仍存在于上游管理"的上游名集合：
// DB 现存（上游已 DB 化，权威来源）∪ 内存配置，二者并集视为"仍存在"。
// 查询 DB 失败时降级为空集（不过滤），避免误吞全部数据。
func (h *Handler) existingUpstreamSet() map[string]bool {
	set := make(map[string]bool)
	if h.store != nil {
		if rows, err := h.store.ListUpstreams(); err == nil {
			for _, u := range rows {
				set[u.Name] = true
			}
		}
	}
	if h.cfg != nil {
		for i := range h.cfg.Upstreams {
			set[h.cfg.Upstreams[i].Name] = true
		}
	}
	return set
}

// MetricsByAPIKey 返回按下游 API Key 聚合的统计（支持 ?range=...）。
// 用于区分不同 AI 程序/客户端的使用量，看哪个 Key 用得多。
// MetricsByAPIKey 返回按下游 API Key 聚合的统计（支持 ?range=...）。
// 历史天走 daily_stats 预聚合，今天实时聚合，结果缓存 60s。
func (h *Handler) MetricsByAPIKey(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		h.writeCached(w, r, []apiKeyMetrics{})
		return
	}
	loc := time.FixedZone("CST", 8*3600)
	agg, err := h.store.AggregateRange(metricsRangeStr(r), loc)
	if err != nil {
		h.writeCached(w, r, []apiKeyMetrics{})
		return
	}
	out := make([]apiKeyMetrics, 0, len(agg.ByAPIKey))
	for name, d := range agg.ByAPIKey {
		m := apiKeyMetrics{
			Name:        name,
			Requests:    d.Requests,
			Successes:   d.Successes,
			TotalTokens: d.Tokens,
			CacheHit:    d.CacheHitTokens,
			CacheMiss:   d.CacheMissTokens,
		}
		if d.Requests > 0 {
			m.SuccessRate = float64(d.Successes) / float64(d.Requests)
			m.AvgLatencyMS = float64(d.SumDurationMS) / float64(d.Requests)
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TotalTokens > out[j].TotalTokens })
	h.writeCached(w, r, out)
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

// MetricsHourly 返回 24h 逐小时趋势（固定查最近 24h，加 60s 缓存）。
func (h *Handler) MetricsHourly(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		h.writeCached(w, r, []hourlyBucket{})
		return
	}
	rows, err := h.store.DB().Query(`
		SELECT strftime('%Y-%m-%dT%H:00:00Z', ts) as hour,
		       COUNT(*) as requests,
		       COALESCE(SUM(CASE WHEN status < 400 THEN 1 ELSE 0 END), 0) as successes,
		       COALESCE(SUM(tokens), 0) as tokens
		FROM request_log WHERE ts >= strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-1 day')
		GROUP BY hour ORDER BY hour ASC
	`)
	if err != nil {
		h.writeCached(w, r, []hourlyBucket{})
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
	h.writeCached(w, r, out)
}

// MetricsDaily 返回按天聚合的趋势（支持 ?range=today|3d|7d|30d|all）。
// 历史天走 daily_stats 预聚合，今天实时聚合，缺失天补零保证折线图连续，结果缓存 60s。
func (h *Handler) MetricsDaily(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		h.writeCached(w, r, []dailyBucket{})
		return
	}
	rangeStr := metricsRangeStr(r)
	loc := time.FixedZone("CST", 8*3600)
	agg, err := h.store.AggregateRange(rangeStr, loc)
	if err != nil {
		h.writeCached(w, r, []dailyBucket{})
		return
	}
	dayMap := map[string]dailyBucket{}
	for _, dp := range agg.Days {
		dayMap[dp.Date] = dailyBucket{Date: dp.Date, Requests: dp.Requests, Successes: dp.Successes, Tokens: dp.Tokens}
	}
	now := time.Now().In(loc)
	start := rangeStart(rangeStr, loc)
	if rangeStr == "all" {
		if len(agg.Days) > 0 {
			if t, e := time.ParseInLocation("2006-01-02", agg.Days[0].Date, loc); e == nil {
				start = t
			}
		} else {
			start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
		}
	}
	var out []dailyBucket
	for d := start; !d.After(now); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		if b, ok := dayMap[key]; ok {
			out = append(out, b)
		} else {
			out = append(out, dailyBucket{Date: key})
		}
	}
	h.writeCached(w, r, out)
}

// MetricsCost 返回费用统计（GET /admin/metrics/cost，支持 ?range=today|3d|7d|30d|all）。
// 历史天走 daily_stats 预聚合，今天实时聚合，结果缓存 60s。
func (h *Handler) MetricsCost(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		h.writeCached(w, r, costMetricsResponse{})
		return
	}
	loc := time.FixedZone("CST", 8*3600)
	agg, err := h.store.AggregateRange(metricsRangeStr(r), loc)
	if err != nil {
		h.writeCached(w, r, costMetricsResponse{})
		return
	}
	resp := costMetricsResponse{
		TotalCost:  agg.Cost,
		ByUpstream: dimToCostRows(agg.ByUpstream),
		ByAPIKey:   dimToCostRows(agg.ByAPIKey),
		ByModel:    dimToCostRows(agg.ByModel),
	}
	h.writeCached(w, r, resp)
}
