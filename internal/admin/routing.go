package admin

import (
	"encoding/json"
	"net/http"

	"github.com/icefairy/xuanji/internal/store"
)

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
					Model:          r.Model,
					Strategy:       strategy,
					Upstreams:      upstreams,
					FastFail:       ff,
					Enabled:        en,
					HealthState:    hs,
					Vision:         r.Vision == 1,
					VisionFallback: r.VisionFallback,
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
			Model:          rule.Model,
			Strategy:       strategy,
			Upstreams:      upstreams,
			FastFail:       ff,
			Enabled:        en,
			HealthState:    hs,
			Vision:         rule.Vision,
			VisionFallback: rule.VisionFallback,
		})
	}
	writeJSON(w, resp)
}

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
