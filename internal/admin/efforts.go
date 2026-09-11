package admin

import (
	"encoding/json"
	"net/http"

	"github.com/icefairy/xuanji/internal/store"
)

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
