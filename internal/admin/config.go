package admin

import (
	"encoding/json"
	"net/http"
)

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

// Config 返回配置摘要：端口以及端点清单。
func (h *Handler) Config(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, configResponse{
		Server: serverSummary{
			Port: h.cfg.Server.Port,
		},
		DefaultStrategy: h.cfg.Routing.DefaultStrategy,
		Endpoints:       endpoints,
	})
}

func (h *Handler) GetRetryConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]interface{}{
		"max_retries":       h.cfg.Retry.MaxRetries,
		"retry_statuses":    h.cfg.Retry.RetryStatuses,
		"retry_keywords":    h.cfg.Retry.RetryKeywords,
		"fast_fail_minutes": h.cfg.Retry.FastFailMinutes,
	})
}

// sensitiveConfigKeys 是不通过管理 API 回显的敏感配置键。
// admin.jwt_secret 用于签发管理员 JWT：一旦回显，持有"AI 助手管理 API key"
// 者即可读到签名密钥，伪造任意管理员令牌完成提权（openapi 的 /api/admin/config
// 走的是 adminKeyAuth，权限低于管理员 JWT）。该密钥仅供服务端内部使用，前端无需展示。
var sensitiveConfigKeys = map[string]bool{
	"admin.jwt_secret": true,
}

// GetAllConfig 返回配置表中所有 key-value 对（GET /admin/config）。
// 敏感键（如 JWT 签名密钥）在此处剔除，避免通过管理 API 泄露。
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
	for k := range sensitiveConfigKeys {
		delete(all, k)
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
