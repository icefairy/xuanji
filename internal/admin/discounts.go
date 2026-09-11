package admin

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/icefairy/xuanji/internal/store"
)

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
