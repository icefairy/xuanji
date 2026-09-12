package admin

import (
	"net/http"
	"strconv"
	"strings"
)

// RequestLogs 返回最近请求日志（GET /admin/logs?limit=50&offset=0&upstream=xx&model=yy&endpoint=zz）。
// 支持按上游/模型/端点筛选与分页；响应含 total、筛选选项
// （客户端程序识别功能已删除，不再有 profile_map——请求日志以 api_key 区分调用方）。
func (h *Handler) RequestLogs(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, map[string]interface{}{"total": 0, "limit": 50, "offset": 0, "logs": []map[string]interface{}{}})
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
		// 同时匹配客户端模型名与上游真实模型名：日志「模型」列显示真实名，
		// 但用户可能按客户端名筛选（反之亦然），两者都命中才符合直觉。
		where += " AND (model = ? OR upstream_model = ?)"
		args = append(args, m, m)
	}
	// 端点筛选：识别还在调用老版接口（如 /v1/completions → endpoint=completions）的程序
	if ep := r.URL.Query().Get("endpoint"); ep != "" {
		where += " AND endpoint = ?"
		args = append(args, ep)
	}
	// 状态筛选：normal=2xx 正常，error=非 2xx（4xx/5xx/499 等异常）
	if st := r.URL.Query().Get("status_type"); st == "normal" {
		where += " AND status >= 200 AND status < 300"
	} else if st == "error" {
		where += " AND (status < 200 OR status >= 300)"
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
				SELECT ts, upstream, model, endpoint, status, duration_ms, prompt_tokens, completion_tokens, tokens, prompt_cache_hit_tokens, prompt_cache_miss_tokens, api_key, cost, upstream_model, client_addr, user_agent, error_detail
				FROM request_log`+where+` ORDER BY id DESC LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		writeJSON(w, map[string]interface{}{"total": total, "limit": limit, "offset": offset, "logs": []map[string]interface{}{}})
		return
	}
	defer rows.Close()

	var out []map[string]interface{}
	for rows.Next() {
		var ts, upstream, model, endpoint, apiKey, upstreamModel, clientAddr, userAgent, errorDetail string
		var status, durationMs, promptTokens, completionTokens, tokens, cacheHitTokens, cacheMissTokens int64
		var cost float64
		if err := rows.Scan(&ts, &upstream, &model, &endpoint, &status, &durationMs, &promptTokens, &completionTokens, &tokens, &cacheHitTokens, &cacheMissTokens, &apiKey, &cost, &upstreamModel, &clientAddr, &userAgent, &errorDetail); err != nil {
			continue
		}
		out = append(out, map[string]interface{}{"ts": fmtCST(ts),
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
			"error_detail":             errorDetail,
		})
	}

	// 筛选选项：日志中出现过的上游、模型与端点（distinct）
	upstreams := []string{}
	models := []string{}
	endpoints := []string{}
	if rows2, err := h.store.DB().Query("SELECT DISTINCT upstream FROM request_log ORDER BY upstream"); err == nil {
		for rows2.Next() {
			var v string
			if rows2.Scan(&v) == nil && v != "" {
				upstreams = append(upstreams, v)
			}
		}
		rows2.Close()
	}
	if rows3, err := h.store.DB().Query("SELECT DISTINCT model FROM request_log " +
		"UNION SELECT DISTINCT upstream_model FROM request_log WHERE upstream_model != '' " +
		"ORDER BY 1"); err == nil {
		for rows3.Next() {
			var v string
			if rows3.Scan(&v) == nil && v != "" {
				models = append(models, v)
			}
		}
		rows3.Close()
	}
	if rows4, err := h.store.DB().Query("SELECT DISTINCT endpoint FROM request_log ORDER BY endpoint"); err == nil {
		for rows4.Next() {
			var v string
			if rows4.Scan(&v) == nil && v != "" {
				endpoints = append(endpoints, v)
			}
		}
		rows4.Close()
	}

	writeJSON(w, map[string]interface{}{
		"total":   total,
		"limit":   limit,
		"offset":  offset,
		"logs":    out,
		"filters": map[string]interface{}{"upstreams": upstreams, "models": models, "endpoints": endpoints},
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
