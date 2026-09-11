package admin

import (
	"encoding/json"
	"io"
	"net/http"
)

// TokenLimits 列出所有模型 token 上限记录（GET /admin/token-limits）。
func (h *Handler) TokenLimits(w http.ResponseWriter, _ *http.Request) {
	if h.store == nil {
		writeJSON(w, map[string]string{"error": "store not available"})
		return
	}
	all, err := h.store.ListTokenLimits()
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"items": all})
}

// DeleteTokenLimit 删除一条模型 token 上限记录（DELETE /admin/token-limits/{upstream}/{model}）。
// 删除后程序会在再次遇到同类 400 错误时自动重新学习并写入。
func (h *Handler) DeleteTokenLimit(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, map[string]string{"error": "store not available"})
		return
	}
	upstream := r.PathValue("upstream")
	model := r.PathValue("model")
	if upstream == "" || model == "" {
		writeJSON(w, map[string]string{"error": "upstream and model are required"})
		return
	}
	if err := h.store.DeleteTokenLimit(upstream, model); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]string{"status": "deleted"})
}

// UpsertTokenLimit 保存模型 token 上限（POST /admin/token-limits），管理员手动编辑后生效。
// 请求体：{upstream, upstream_model, max_completion_tokens?, max_tokens?, source?}
// source 默认 "manual"，可省略。upstream_model 为空时用 upstream_model 字段兜底（同请求体）。
func (h *Handler) UpsertTokenLimit(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, map[string]string{"error": "store not available"})
		return
	}
	var req struct {
		Upstream            string `json:"upstream"`
		UpstreamModel       string `json:"upstream_model"`
		MaxCompletionTokens int    `json:"max_completion_tokens"`
		MaxTokens           int    `json:"max_tokens"`
		Source              string `json:"source"`
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, map[string]string{"error": "read body failed"})
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if req.Upstream == "" || req.UpstreamModel == "" {
		writeJSON(w, map[string]string{"error": "upstream and upstream_model are required"})
		return
	}
	if req.Source == "" {
		req.Source = "manual"
	}
	if err := h.store.UpsertTokenLimit(req.Upstream, req.UpstreamModel, req.MaxCompletionTokens, req.MaxTokens, req.Source); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}
