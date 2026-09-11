package admin

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/icefairy/xuanji/internal/store"
)

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
