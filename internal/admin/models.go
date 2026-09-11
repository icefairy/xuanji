package admin

import (
	"net/http"
	"time"
)

// Models 返回 OpenAI 兼容的模型列表（GET /v1/models）。
func (h *Handler) Models(w http.ResponseWriter, _ *http.Request) {
	type modelObj struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	var models []modelObj
	seen := make(map[string]bool)
	for _, rule := range h.cfg.Routing.Rules {
		if !seen[rule.Model] {
			seen[rule.Model] = true
			models = append(models, modelObj{
				ID:      rule.Model,
				Object:  "model",
				Created: time.Now().Unix(),
				OwnedBy: "xuanji",
			})
		}
	}
	writeJSON(w, map[string]interface{}{
		"object": "list",
		"data":   models,
	})
}
