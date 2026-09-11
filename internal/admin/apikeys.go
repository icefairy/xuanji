package admin

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/icefairy/xuanji/internal/store"
)

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
	h.refreshQuota()
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
	h.refreshQuota()
	writeJSON(w, map[string]string{"status": "ok"})
}

// RenameAPIKey 修改下游 API Key 的名称（PUT /admin/api-keys/{id}/name）。
// 请求体：{"name":"新名称"}。改名后同步刷新鉴权缓存，并把 request_log 中
// 该 key 的历史记录名称快照一并更新（保持请求日志显示一致）。
func (h *Handler) RenameAPIKey(w http.ResponseWriter, r *http.Request) {
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
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	// 先取旧名（用于同步历史 request_log 快照）
	oldName := ""
	if tokens, err := h.store.ListAPITokens(); err == nil {
		for _, t := range tokens {
			if t.ID == uint(id) {
				oldName = t.Name
				break
			}
		}
	}
	if err := h.store.UpdateAPITokenName(uint(id), req.Name); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	// 同步历史 request_log 名称快照（旧名 → 新名）
	if oldName != "" && oldName != req.Name {
		if err := h.store.RenameLogAPIKey(oldName, strings.TrimSpace(req.Name)); err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
	}
	h.refreshAuth()
	h.refreshQuota()
	writeJSON(w, map[string]string{"status": "ok"})
}

// UpdateAPIKeyPolicy 更新下游 key 的分组归属与覆盖策略（PUT /admin/api-keys/{id}/policy）。
// 请求体（全部为改动的字段，未传的保留）：
//
//	{"group_id": 3, "allowed_models": "[...]", "quota_override": "{...}"}
//
// 传 group_id 0 表示脱离组；allowed_models/quota_override 传空串表示不修改。
func (h *Handler) UpdateAPIKeyPolicy(w http.ResponseWriter, r *http.Request) {
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
		GroupID       uint   `json:"group_id"`
		AllowedModels string `json:"allowed_models"`
		QuotaOverride string `json:"quota_override"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	if err := h.store.UpdateAPITokenPolicy(uint(id), req.GroupID, req.AllowedModels, req.QuotaOverride); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	h.refreshQuota()
	writeJSON(w, map[string]string{"status": "ok"})
}
