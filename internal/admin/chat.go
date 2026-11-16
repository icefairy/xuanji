package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/tidwall/gjson"
)

// Chat 处理 POST /admin/chat：多轮对话调试，走完整网关路由链路。
// 复用 proxy.Handler.ChatCompletions（路由选择/模型映射/vision fallback/健康检查/
// 日志记录），非直连上游。图片转 OpenAI 标准 image_url content，网关 vision fallback
// 自动识别。响应含 reply/usage/routing（routing 尽力而为，供前端展示）。
func (h *Handler) Chat(w http.ResponseWriter, r *http.Request) {
	if h.px == nil {
		writeJSON(w, map[string]any{"error": "对话调试不可用：转发链路未注入（proxy 为 nil）"})
		return
	}
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "请求体解析失败: " + err.Error()})
		return
	}
	if req.Model == "" {
		req.Model = "deepseek-v4-flash"
	}
	if len(req.Messages) == 0 {
		writeJSON(w, map[string]string{"error": "messages 不能为空"})
		return
	}
	if req.MaxTokens <= 0 {
		req.MaxTokens = 1024
	}
	// 图片：把最后一条 user 消息的 content 从 string 改为多模态数组
	messages, _ := buildChatMessages(req.Messages, req.Images)

	// 组装 OpenAI chat body（采样参数仅非零时携带；0 表示不传）
	body := map[string]any{
		"model":      req.Model,
		"messages":   messages,
		"max_tokens": req.MaxTokens,
	}
	if req.Temperature != 0 {
		body["temperature"] = req.Temperature
	}
	if req.TopP != 0 {
		body["top_p"] = req.TopP
	}
	if req.TopK > 0 {
		body["top_k"] = req.TopK
	}
	if req.RepetitionPenalty > 0 {
		body["repetition_penalty"] = req.RepetitionPenalty
	}
	if req.FrequencyPenalty != 0 {
		body["frequency_penalty"] = req.FrequencyPenalty
	}
	if req.ReasoningEffort != "" {
		body["reasoning_effort"] = req.ReasoningEffort
	}
	payload, _ := json.Marshal(body)

	// 构造合成请求走完整转发链路（复用 ChatCompletions，含 vision fallback 与日志记录）
	// 带调试标记头：proxy 会把实际命中的上游与真实模型名写进响应自定义 header
	// （X-Xuanji-Upstream / X-Xuanji-Upstream-Model），供 routing 展示——保证展示的就是
	// 实际转发（含重试后）真正打中的上游，而非转发前再按规则选一次。
	start := time.Now()
	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		writeJSON(w, map[string]string{"error": "构造请求失败: " + err.Error()})
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Xuanji-Debug-Channel", "1")
	cw := &captureWriter{header: http.Header{}}
	h.px.ChatCompletions(cw, httpReq)
	durationMS := time.Since(start).Milliseconds()

	routing := map[string]any{"status": cw.status, "duration_ms": durationMS}
	// 从响应自定义 header 读实际命中的上游与真实模型名
	if up := cw.header.Get("X-Xuanji-Upstream"); up != "" {
		routing["upstream"] = up
	}
	if um := cw.header.Get("X-Xuanji-Upstream-Model"); um != "" {
		routing["upstream_model"] = um
	}

	respBody := cw.body.Bytes()
	if cw.status >= 300 || cw.status == 0 {
		msg := gjson.GetBytes(respBody, "error.message").String()
		if msg == "" {
			msg = "网关转发失败（HTTP " + strconv.Itoa(cw.status) + "）"
		}
		writeJSON(w, map[string]any{"error": msg, "status": cw.status, "routing": routing})
		return
	}
	writeJSON(w, map[string]any{
		"reply":   extractChatReply(respBody),
		"usage":   extractChatUsage(respBody),
		"routing": routing,
	})
}
