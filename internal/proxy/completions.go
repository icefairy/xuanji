// 老版 OpenAI 接口兼容：POST /v1/completions（text 补全，非 chat）。
// 老程序仍调用废弃的 completions 接口时，网关把 completions 请求转换为
// chat/completions 格式转发上游，再把上游 chat 响应转回 completions 格式返回。
// 转换逻辑全部独立在本文件，不影响现有 chat 链路；日志 endpoint 记为 "completions"
//（与 chat 区分，便于在请求日志里按端点筛选出仍调用老接口的程序）。
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/store"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Completions 处理 POST /v1/completions（OpenAI 老版 text 补全接口）。
// 请求体：{model, prompt(string|string[]), max_tokens, temperature, top_p, stop, stream, ...}
//   - prompt 为 string 或 string 数组，统一拼合为一条 user 消息
//   - 流式（stream=true）首版不支持，返回明确 400
//   - 其余采样参数原样带到 chat 请求（chat 兼容字段子集）
func (h *Handler) Completions(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w}
	start := time.Now()

	var model, upstream string
	defer func() {
		h.log.Info("completions",
			"method", r.Method,
			"path", r.URL.Path,
			"model", model,
			"upstream", upstream,
			"status", rec.status,
			"duration", time.Since(start).String(),
		)
	}()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(rec, http.StatusBadRequest, "failed to read request body", "invalid_request_error", "invalid_request")
		return
	}
	if !gjson.ValidBytes(body) {
		writeError(rec, http.StatusBadRequest, "invalid request body", "invalid_request_error", "invalid_request")
		return
	}
	model = gjson.GetBytes(body, "model").String()
	if model == "" {
		writeError(rec, http.StatusBadRequest, "model is required", "invalid_request_error", "missing_model")
		return
	}
	if gjson.GetBytes(body, "stream").Bool() {
		// 首版不做流式转换：明确报错而非静默失败
		writeError(rec, http.StatusBadRequest, "stream not supported for /v1/completions", "invalid_request_error", "stream_not_supported")
		return
	}

	chatBody, err := completionsToChatBody(body)
	if err != nil {
		writeError(rec, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_prompt")
		return
	}

	upstreams, strategy, err := h.router.Route(model)
	if err != nil {
		writeError(rec, http.StatusNotFound, fmt.Sprintf("no upstream found for model %q", model), "invalid_request_error", "model_not_found")
		return
	}

	candidates := h.selectCandidates(upstreams, strategy, model)
	connIssues := false // 是否有连接类错误（网络断/超时），用于判断全局网络问题
	for i := 0; i < len(candidates); i++ {
		// 客户端已断开（context canceled）：不再尝试任何上游，也不标记 fastfail
		if r.Context().Err() != nil {
			h.log.Warn("client disconnected, aborting completions retry loop",
				"model", model, "error", r.Context().Err())
			break
		}
		up := candidates[i]
		handled, retryable, ferr := h.forwardCompletion(rec, r, chatBody, up, model)
		if ferr != nil && h.health != nil {
			h.health.MarkFailure(up.Name)
		}
		if ferr != nil {
			var netErr net.Error
			if errors.As(ferr, &netErr) || errors.Is(ferr, context.DeadlineExceeded) {
				connIssues = true
			}
		}
		if handled {
			upstream = up.Name
			return
		}
		h.log.Warn("completions upstream failed, trying next",
			"upstream", up.Name, "model", model, "error", ferr)
		if !retryable {
			break
		}
	}
	// 全部候选失败 + 含连接类错误 = 全局网络问题，清空黑名单让网络恢复后立即可用
	//（与 chat/rerank/embedding 链路一致）
	if connIssues && h.fastFail != nil {
		cleared := 0
		for _, up := range candidates {
			h.clearUpstreamBlacklist(up, model)
			cleared++
		}
		if cleared > 0 {
			h.log.Warn("all completions upstreams failed with connection errors (likely network issue), cleared fastfail blacklist",
				"model", model, "cleared", cleared)
		}
	}
	writeError(rec, http.StatusBadGateway, "all upstreams failed", "server_error", "upstream_unreachable")
}

// completionsToChatBody 把 completions 请求体转换为 chat/completions 请求体：
//   - prompt（string 或 string 数组）→ messages: [{role:"user", content: 拼合}]
//   - 采样/通用参数子集原样搬运（model、max_tokens、temperature、top_p、stop、n、
//     presence_penalty、frequency_penalty、logit_bias、logprobs、top_logprobs、seed、user）
//   - completions 独有字段（suffix、echo、best_of 等）丢弃，避免 chat 上游 400
func completionsToChatBody(body []byte) ([]byte, error) {
	model := gjson.GetBytes(body, "model").String()
	prompt, err := extractPrompt(body)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
	}
	for _, k := range []string{
		"max_tokens", "temperature", "top_p", "stop", "n",
		"presence_penalty", "frequency_penalty", "logit_bias",
		"logprobs", "top_logprobs", "seed", "user",
	} {
		v := gjson.GetBytes(body, k)
		if !v.Exists() || v.Raw == "" || v.Raw == "null" {
			continue
		}
		var raw any
		if json.Unmarshal([]byte(v.Raw), &raw) == nil {
			out[k] = raw
		}
	}
	return json.Marshal(out)
}

// extractPrompt 提取 completions 请求的 prompt：string 原样返回；
// string 数组按行拼合（OpenAI 老接口数组语义是多个序列，此处按需求统一拼为一条 user 消息）。
func extractPrompt(body []byte) (string, error) {
	p := gjson.GetBytes(body, "prompt")
	if !p.Exists() || (p.Type == gjson.Null) {
		return "", errors.New("prompt is required")
	}
	if p.IsArray() {
		var parts []string
		p.ForEach(func(_, v gjson.Result) bool {
			parts = append(parts, v.String())
			return true
		})
		return strings.Join(parts, "\n"), nil
	}
	return p.String(), nil
}

// forwardCompletion 向单个上游转发一次转换后的 chat 请求（OpenAI 兼容非流式），
// 并把上游 chat 响应转换为 completions 格式返回。日志 endpoint 记为 "completions"。
func (h *Handler) forwardCompletion(w http.ResponseWriter, r *http.Request, body []byte, up *config.Upstream, model string) (handled, retryable bool, err error) {
	start := time.Now()
	var status int
	var promptTokens, completionTokens int64
	defer func() {
		if h.recorder == nil || !handled {
			return
		}
		if sr, ok := w.(*statusRecorder); ok {
			status = sr.status
		}
		h.recorder.Record(store.Record{
			Timestamp:        time.Now(),
			Upstream:         up.Name,
			Model:            model,
			Endpoint:         "completions",
			Status:           status,
			DurationMS:       time.Since(start).Milliseconds(),
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			Tokens:           promptTokens + completionTokens,
			APIKey:           h.recordAPIKey(r),
			ClientAddr:       r.RemoteAddr,  // 客户端地址 "IP:port"，用于区分调用程序
			UserAgent:        r.UserAgent(), // 客户端 UA，程序识别最强信号
		})
	}()

	reqBody := body
	upstreamModel := h.pickAvailableModel(up, model)
	if upstreamModel == "" {
		// fallback：全黑名单时按原 MapModel 随机选一个真实模型继续尝试（连接错误可清黑名单）
		upstreamModel = h.router.MapModel(up, model)
	}
	if upstreamModel != model {
		if reqBody, err = sjson.SetBytes(body, "model", upstreamModel); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to rewrite model field", "server_error", "")
			return true, false, nil
		}
	}

	// 与 forwardOnce 一致：BaseURL 末尾已是完整版本前缀（如 /v1），直接拼 /chat/completions
	target := strings.TrimRight(up.BaseURL, "/") + "/chat/completions"
	reqCtx, cancel := context.WithTimeout(r.Context(), upstreamTimeoutFor(h.cfg))
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target, bytes.NewReader(reqBody))
	if err != nil {
		return false, true, fmt.Errorf("build completions request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+up.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		// 客户端断连（context.Canceled）不算上游故障，不标记 fastfail
		if !errors.Is(err, context.Canceled) && h.fastFail != nil {
			h.fastFail.MarkFailedWithReason(up.Name, upstreamModel, "request failed: "+err.Error())
		}
		return false, true, fmt.Errorf("completions upstream request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		shouldRetry := false
		for _, code := range h.cfg.Retry.RetryStatuses {
			if resp.StatusCode == code {
				shouldRetry = true
				break
			}
		}
		if shouldRetry {
			if h.fastFail != nil {
				h.fastFail.MarkFailedWithReason(up.Name, upstreamModel, fmt.Sprintf("status=%d", resp.StatusCode))
			}
			return false, true, fmt.Errorf("completions upstream error: %s", resp.Status)
		}
		// 不可重试：上游错误本身已是 OpenAI 错误格式，直接透传
		h.writeUpstreamError(w, resp)
		return true, false, fmt.Errorf("completions upstream error: %s", resp.Status)
	}
	if h.fastFail != nil {
		h.fastFail.MarkSuccess(up.Name, upstreamModel)
	}
	respBody, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		h.log.Debug("read completions upstream body", "error", rerr)
	}
	parseUsage(respBody, &promptTokens, &completionTokens, nil, nil)
	// 空内容完成（思考型 max_tokens 不足被截断，content 空 + finish_reason=length）：
	// HTTP 200 但响应无效，非最后候选时切换下一个（与 chat 链路一致）
	if IsEmptyCompletion(respBody) {
		if h.fastFail != nil {
			h.fastFail.MarkFailedWithReason(up.Name, upstreamModel, "empty completion")
		}
		return false, true, errors.New("empty completion (thinking truncated?)")
	}
	outBody, cerr := chatToCompletionResponse(respBody, upstreamModel)
	if cerr != nil {
		// 转换失败（上游响应异常）：原样透传 chat 响应，避免客户端拿不到任何东西
		h.log.Warn("completions response convert failed, passthrough raw",
			"upstream", up.Name, "error", cerr)
		copyHeader(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
		return true, false, nil
	}
	copyHeader(w.Header(), resp.Header)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(outBody)
	return true, false, nil
}

// completionResponse 是 OpenAI 老版 text_completion 响应结构。
type completionResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []completionChoice `json:"choices"`
	Usage   *completionUsage   `json:"usage,omitempty"`
}

type completionChoice struct {
	Text         string `json:"text"`
	Index        int    `json:"index"`
	FinishReason string `json:"finish_reason"`
}

type completionUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// chatToCompletionResponse 把上游 chat/completions 响应转换为 completions 格式：
// choices[].message.content → choices[].text，object 改为 "text_completion"。
func chatToCompletionResponse(chatBody []byte, fallbackModel string) ([]byte, error) {
	var chat struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Index        int    `json:"index"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(chatBody, &chat); err != nil {
		return nil, err
	}
	out := completionResponse{ID: chat.ID, Object: "text_completion", Created: chat.Created, Model: chat.Model}
	if out.Model == "" {
		out.Model = fallbackModel
	}
	for _, c := range chat.Choices {
		out.Choices = append(out.Choices, completionChoice{
			Text:         c.Message.Content,
			Index:        c.Index,
			FinishReason: c.FinishReason,
		})
	}
	if chat.Usage != nil {
		out.Usage = &completionUsage{
			PromptTokens:     chat.Usage.PromptTokens,
			CompletionTokens: chat.Usage.CompletionTokens,
			TotalTokens:      chat.Usage.TotalTokens,
		}
	}
	return json.Marshal(out)
}
