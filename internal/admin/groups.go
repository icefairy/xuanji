package admin

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/icefairy/xuanji/internal/quota"
)

// Groups 列出所有组及模型配额矩阵（GET /admin/groups）。
func (h *Handler) Groups(w http.ResponseWriter, _ *http.Request) {
	if h.store == nil {
		writeJSON(w, []groupView{})
		return
	}
	groups, err := h.store.ListGroups()
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	now := time.Now().UTC()
	quotaStart := func(win string) time.Time {
		switch win {
		case "week":
			return quota.WeekStart(now)
		case "month":
			return quota.MonthStart(now)
		default:
			return now.Add(-5 * time.Hour)
		}
	}
	out := make([]groupView, 0, len(groups))
	for i := range groups {
		g := &groups[i]
		v := groupView{ID: g.ID, Name: g.Name, AllowedModels: g.AllowedModels, Remark: g.Remark, MemberCount: g.MemberCount, Quotas: []groupQuotaView{}}
		qrows, err := h.store.ListGroupQuotas(g.ID)
		if err != nil {
			continue
		}
		for _, q := range qrows {
			qv := groupQuotaView{Model: q.Model, Token5H: q.Token5H, TokenWeek: q.TokenWeek, TokenMonth: q.TokenMonth}
			qv.Used5H, _ = h.store.GroupWindowTokenSum(g.ID, q.Model, quotaStart("5h"))
			qv.UsedWeek, _ = h.store.GroupWindowTokenSum(g.ID, q.Model, quotaStart("week"))
			qv.UsedMonth, _ = h.store.GroupWindowTokenSum(g.ID, q.Model, quotaStart("month"))
			v.Quotas = append(v.Quotas, qv)
		}
		out = append(out, v)
	}
	writeJSON(w, out)
}

// CreateGroup 创建组（POST /admin/groups）。请求体：{"name","allowed_models","remark"}
func (h *Handler) CreateGroup(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, map[string]string{"error": "store not available"})
		return
	}
	var req struct {
		Name          string `json:"name"`
		AllowedModels string `json:"allowed_models"`
		Remark        string `json:"remark"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	g, err := h.store.CreateGroup(req.Name, req.AllowedModels, req.Remark)
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	h.refreshQuota()
	writeJSON(w, map[string]interface{}{"status": "ok", "id": g.ID})
}

// UpdateGroup 更新组信息（PUT /admin/groups/{id}）。空字段表示不修改。
func (h *Handler) UpdateGroup(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, map[string]string{"error": "store not available"})
		return
	}
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 32)
	if err != nil {
		writeJSON(w, map[string]string{"error": "invalid id"})
		return
	}
	var req struct {
		Name          string `json:"name"`
		AllowedModels string `json:"allowed_models"`
		Remark        string `json:"remark"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := h.store.UpdateGroup(uint(id), req.Name, req.AllowedModels, req.Remark); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	h.refreshQuota()
	writeJSON(w, map[string]string{"status": "ok"})
}

// DeleteGroup 删除组（DELETE /admin/groups/{id}）。
func (h *Handler) DeleteGroup(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, map[string]string{"error": "store not available"})
		return
	}
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 32)
	if err != nil {
		writeJSON(w, map[string]string{"error": "invalid id"})
		return
	}
	if err := h.store.DeleteGroup(uint(id)); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	h.refreshQuota()
	writeJSON(w, map[string]string{"status": "ok"})
}

// UpdateGroupQuota 设置某组某模型的配额（PUT /admin/groups/{id}/quotas）。
// 请求体：{"model","token_5h","token_week","token_month"}；三窗口全 0 视为删除该行。
func (h *Handler) UpdateGroupQuota(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, map[string]string{"error": "store not available"})
		return
	}
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 32)
	if err != nil {
		writeJSON(w, map[string]string{"error": "invalid id"})
		return
	}
	var req struct {
		Model      string `json:"model"`
		Token5H    int64  `json:"token_5h"`
		TokenWeek  int64  `json:"token_week"`
		TokenMonth int64  `json:"token_month"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	if err := h.store.UpsertGroupQuota(uint(id), req.Model, req.Token5H, req.TokenWeek, req.TokenMonth); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	h.refreshQuota()
	writeJSON(w, map[string]string{"status": "ok"})
}

// DeleteGroupQuota 删除某组某模型的配额（DELETE /admin/groups/{id}/quotas/{model}）。
func (h *Handler) DeleteGroupQuota(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, map[string]string{"error": "store not available"})
		return
	}
	id, _ := strconv.ParseUint(r.PathValue("id"), 10, 32)
	model := r.PathValue("model")
	if model == "" {
		writeJSON(w, map[string]string{"error": "model is required"})
		return
	}
	if err := h.store.DeleteGroupQuota(uint(id), model); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	h.refreshQuota()
	writeJSON(w, map[string]string{"status": "ok"})
}
