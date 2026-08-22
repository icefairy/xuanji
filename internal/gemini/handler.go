// Package gemini 实现 Google Gemini 上游支持：OpenAI 协议入口（/v1/chat/completions）
// 路由到 gemini 类型上游时，请求转 Gemini 格式转发，响应转回 OpenAI。
package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/health"
	"github.com/icefairy/xuanji/internal/router"
	"github.com/icefairy/xuanji/internal/store"
)

// upstreamTimeout 是上游连接与（非流式）整体请求的超时时间。
const upstreamTimeout = 120 * time.Second

// genPath 拼在 upstream.BaseURL 之后。Gemini REST 的模型端点：
// {base}/models/{model}:generateContent 与 :streamGenerateContent。
// BaseURL 约定形如 https://generativelanguage.googleapis.com/v1beta。
func genPath(model, stream string) string {
	return "/models/" + model + ":" + stream
}

// Handler 处理 OpenAI 入口路由到 Gemini 上游的转换。
type Handler struct {
	router   *router.Router
	health   *health.Checker
	client   *http.Client
	log      *slog.Logger
	recorder *store.Recorder
	timeout  time.Duration
	keyName  func(r *http.Request) string
}

// SetKeyName 注入下游 API Key 展示名解析器（统计按 Key 维度用；nil 时记录空）。
func (h *Handler) SetKeyName(fn func(r *http.Request) string) {
	h.keyName = fn
}

func (h *Handler) recordAPIKey(r *http.Request) string {
	if h.keyName == nil {
		return ""
	}
	return h.keyName(r)
}

// SetTimeout 设置上游请求超时（来自 retry.upstream_timeout 配置）。
func (h *Handler) SetTimeout(d time.Duration) {
	if d > 0 {
		h.timeout = d
	}
}

// New 创建 Gemini 上游 Handler。
func New(rt *router.Router, hc *health.Checker) *Handler {
	transport := &http.Transport{
		DialContext: (&net.Dialer{Timeout: upstreamTimeout}).DialContext,
	}
	return &Handler{
		router:  rt,
		health:  hc,
		timeout: upstreamTimeout,
		client:  &http.Client{Transport: transport},
		log:     slog.Default(),
	}
}

// SetRecorder 注入指标记录器（nil 安全；测试不需要调用）。
func (h *Handler) SetRecorder(r *store.Recorder) {
	h.recorder = r
}

// OpenAICompletions 处理 /v1/chat/completions 路由到 Gemini 上游的转换。
func (h *Handler) OpenAICompletions(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w}
	start := time.Now()
	var model string
	var stream bool

	defer func() {
		h.log.Info("openai→gemini",
			"model", model,
			"stream", stream,
			"status", rec.status,
			"duration", time.Since(start).String(),
		)
	}()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(rec, http.StatusBadRequest, "failed to read request body")
		return
	}

	// 解析 model 与 stream
	var req struct {
		Model  string `json:"model"`
		Stream *bool  `json:"stream"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeOpenAIError(rec, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	model = req.Model
	if model == "" {
		writeOpenAIError(rec, http.StatusBadRequest, "model is required")
		return
	}
	stream = req.Stream != nil && *req.Stream

	// 路由 + 健康过滤
	upstreams, _, err := h.router.Route(model)
	if err != nil {
		writeOpenAIError(rec, http.StatusNotFound, fmt.Sprintf("no upstream found for model %q", model))
		return
	}
	candidates := h.selectCandidates(upstreams, model)

	for i, up := range candidates {
		handled, retryable, ferr := h.forwardOnce(rec, r, body, up, model, stream, i == len(candidates)-1)
		if ferr != nil && h.health != nil {
			h.health.MarkFailure(up.Name)
		}
		if handled {
			return
		}
		h.log.Warn("upstream failed, trying next",
			"upstream", up.Name, "model", model, "error", ferr)
		if !retryable {
			break
		}
	}
	writeOpenAIError(rec, http.StatusBadGateway, "all upstreams failed")
}

// selectCandidates 依据健康状态选出转发候选（与 proxy 相同逻辑）。
func (h *Handler) selectCandidates(ups []*config.Upstream, model string) []*config.Upstream {
	if h.health == nil {
		return ups
	}
	healthy := h.health.HealthyUpstreams(ups)
	if len(healthy) > 0 {
		return healthy
	}
	h.log.Warn("no healthy upstream for model, falling back to first",
		"model", model, "total", len(ups))
	return ups[:1]
}

// forwardOnce 向单个 Gemini 上游转发一次 OpenAI 请求。
func (h *Handler) forwardOnce(w http.ResponseWriter, r *http.Request, body []byte, up *config.Upstream, model string, stream, last bool) (handled, retryable bool, err error) {
	start := time.Now()
	var status int
	defer func() {
		if h.recorder == nil || !handled {
			return
		}
		if sr, ok := w.(*statusRecorder); ok {
			status = sr.status
		}
		h.recorder.Record(store.Record{
			Timestamp:  time.Now(),
			Upstream:   up.Name,
			Model:      model,
			Endpoint:   "gemini",
			Status:     status,
			DurationMS: time.Since(start).Milliseconds(),
			Tokens:     0,
			APIKey:     h.recordAPIKey(r),
			ClientAddr: r.RemoteAddr,
			UserAgent:  r.UserAgent(),
		})
	}()

	// 模型映射（model_mapping 只改 model 字段）
	upModel := h.router.MapModel(up, model)

	// 请求转换：OpenAI → Gemini
	reqBody, cerr := OpenAIChatToGemini(body, upModel)
	if cerr != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "failed to convert request: "+cerr.Error())
		return true, false, nil
	}

	action := "generateContent"
	if stream {
		action = "streamGenerateContent"
	}
	target := strings.TrimRight(up.BaseURL, "/") + genPath(upModel, action)

	reqCtx := r.Context()
	if !stream {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(reqCtx, h.timeout)
		defer cancel()
	}
	req, berr := http.NewRequestWithContext(reqCtx, http.MethodPost, target, bytes.NewReader(reqBody))
	if berr != nil {
		if last {
			writeOpenAIError(w, http.StatusInternalServerError, "failed to build upstream request")
			return true, false, nil
		}
		return false, true, fmt.Errorf("build upstream request: %w", berr)
	}
	req.Header.Set("x-goog-api-key", up.APIKey)
	req.Header.Set("Content-Type", "application/json")
	config.ApplyUpstreamUserAgent(req)

	resp, derr := h.client.Do(req)
	if derr != nil {
		if last {
			writeOpenAIError(w, http.StatusBadGateway, "upstream request failed: "+derr.Error())
			return true, false, nil
		}
		return false, true, fmt.Errorf("upstream request failed: %w", derr)
	}
	defer resp.Body.Close()

	switch {
	case stream && resp.StatusCode >= 200 && resp.StatusCode < 300:
		// 流式：Gemini 流 → OpenAI SSE
		h.streamConvert(w, resp)
		return true, false, nil
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		if last {
			h.writeUpstreamOpenAIError(w, resp)
			return true, false, fmt.Errorf("upstream error: %s", resp.Status)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		return false, true, fmt.Errorf("upstream error: %s", resp.Status)
	case resp.StatusCode >= 400:
		h.writeUpstreamOpenAIError(w, resp)
		return true, false, nil
	default:
		// 非流式：Gemini 响应 → OpenAI 响应
		data, rerr := io.ReadAll(resp.Body)
		if rerr != nil {
			writeOpenAIError(w, http.StatusBadGateway, "failed to read upstream response")
			return true, false, nil
		}
		openAIResp, oerr := GeminiResponseToOpenAI(data, model)
		if oerr != nil {
			writeOpenAIError(w, http.StatusBadGateway, "failed to convert response: "+oerr.Error())
			return true, false, nil
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(openAIResp)
		return true, false, nil
	}
}

// streamConvert 把上游 Gemini 流转换为 OpenAI SSE 事件流写回客户端。
func (h *Handler) streamConvert(w http.ResponseWriter, resp *http.Response) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	if err := StreamGeminiToOpenAI(resp.Body, w); err != nil {
		h.log.Debug("stream convert", "error", err)
	}
}

// writeUpstreamOpenAIError 把上游 Gemini 错误响应转为 OpenAI 错误格式。
func (h *Handler) writeUpstreamOpenAIError(w http.ResponseWriter, resp *http.Response) {
	message := ""
	status := "INTERNAL"
	if data, err := io.ReadAll(resp.Body); err == nil {
		var gErr GeminiError
		if json.Unmarshal(data, &gErr) == nil && gErr.Error.Message != "" {
			message = gErr.Error.Message
			status = gErr.Error.Status
		}
	}
	if message == "" {
		message = "upstream error: " + resp.Status
	}
	code := resp.StatusCode
	if code < 400 || code > 599 {
		code = http.StatusBadGateway
	}
	writeOpenAIErrorWithStatus(w, code, message, status)
}

// writeOpenAIError 以 OpenAI 错误格式写响应。
func writeOpenAIError(w http.ResponseWriter, status int, msg string) {
	writeOpenAIErrorWithStatus(w, status, msg, "invalid_request_error")
}

func writeOpenAIErrorWithStatus(w http.ResponseWriter, status int, msg, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"message": msg,
			"type":    errType,
			"code":    fmt.Sprintf("err_%d", status),
		},
	})
}

// statusRecorder 记录实际写出的状态码，供请求日志与指标埋点使用。
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

// WriteHeader 记录状态码并转发，只生效一次。
func (s *statusRecorder) WriteHeader(code int) {
	if s.wrote {
		return
	}
	s.wrote = true
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Write 未显式 WriteHeader 时按 200 处理。
func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.WriteHeader(http.StatusOK)
	}
	return s.ResponseWriter.Write(b)
}
