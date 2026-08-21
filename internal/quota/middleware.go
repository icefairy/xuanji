package quota

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// bearerToken 从请求头提取下游 API key（与 auth.Middleware 同口径）。
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	if xk := r.Header.Get("x-api-key"); xk != "" {
		return strings.TrimSpace(xk)
	}
	return ""
}

// extractModel 从请求体 JSON 顶层提取 "model" 字符串字段；无则不限定配额模型。
// 对 OpenAI chat/completions、Anthropic messages、embeddings 等都有顶层 model。
func extractModel(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	raw, ok := m["model"]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// Middleware 对下游请求做白名单 + 模型级配额检查：
//   - model 解析不出（无 body / 非 JSON）或 token 无策略 → 放行；
//   - 白名单不通过 → 403；
//   - 某模型 5h/周/月 窗口超限 → 429，且不影响其他模型。
//
// 读取 body 后原样写回（bytes.NewReader），下游 handler 可继续读。
func (s *Service) Middleware(next http.HandlerFunc) http.HandlerFunc {
	if s == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.Enabled() {
			next(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err == nil && len(body) > 0 {
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		token := bearerToken(r)
		model := extractModel(body)
		if model == "" {
			next(w, r)
			return
		}
		if qe := s.Check(token, model); qe != nil {
			writeQuotaError(w, r, qe)
			return
		}
		next(w, r)
	}
}

// writeQuotaError 写配额拦截响应（OpenAI 风格错误体）。
func writeQuotaError(w http.ResponseWriter, r *http.Request, qe *QuotaError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(qe.Status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"message": qe.Message,
			"type":    "invalid_request_error",
			"code":    qe.Code,
		},
		"quota": qe.Details,
	})
}
