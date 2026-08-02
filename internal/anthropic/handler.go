// Package anthropic 实现 Anthropic(Claude) 协议入口：请求转 OpenAI 转发，响应转回 Anthropic。
package anthropic

import (
	"bufio"
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
const upstreamTimeout = 60 * time.Second

// chatPath 拼接在 upstream.BaseURL 之后，构成 chat/completions 转发端点。
// ⚠ 2026-08-02 修复：必须带 /v1 前缀——部分上游（基元律动、商汤日日新）只认 /v1/ 前缀，
// 否则返回 405/404。与 proxy 包同样处理 base_url 末尾已有 /v1 的情况（在拼接处判断）。
const chatPath = "/v1/chat/completions"

// Handler 处理 POST /v1/messages（Anthropic 协议）。
type Handler struct {
	router   *router.Router
	health   *health.Checker
	client   *http.Client
	log      *slog.Logger
	recorder *store.Recorder // 指标记录器；nil 时跳过记录
	timeout  time.Duration   // 上游请求超时；默认 60s，可用 SetTimeout 覆盖
	keyName  func(r *http.Request) string // 下游 API Key 展示名（统计用）；nil 时记录空
}

// SetKeyName 注入下游 API Key 展示名解析器（统计按 Key 维度用；nil 时记录空）。
func (h *Handler) SetKeyName(fn func(r *http.Request) string) {
	h.keyName = fn
}

// recordAPIKey 解析当前请求的下游 API Key 展示名（未注入解析器时返回空）。
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

// New 创建 Anthropic 协议 Handler。
func New(rt *router.Router, hc *health.Checker) *Handler {
	transport := &http.Transport{
		DialContext: (&net.Dialer{Timeout: upstreamTimeout}).DialContext,
	}
	return &Handler{
		router: rt,
		health: hc,
		timeout: upstreamTimeout,
		client: &http.Client{Transport: transport},
		log:    slog.Default(),
	}
}

// SetRecorder 注入指标记录器（nil 安全；测试不需要调用）。
func (h *Handler) SetRecorder(r *store.Recorder) {
	h.recorder = r
}

// Messages 处理 POST /v1/messages。
func (h *Handler) Messages(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w}
	start := time.Now()
	var model string
	var stream bool

	defer func() {
		h.log.Info("v1/messages",
			"model", model,
			"stream", stream,
			"status", rec.status,
			"duration", time.Since(start).String(),
		)
	}()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeClaudeError(rec, http.StatusBadRequest, "failed to read request body")
		return
	}

	var claudeReq ClaudeRequest
	if err := json.Unmarshal(body, &claudeReq); err != nil {
		writeClaudeError(rec, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if claudeReq.Model == "" {
		writeClaudeError(rec, http.StatusBadRequest, "model is required")
		return
	}
	model = claudeReq.Model
	stream = claudeReq.Stream != nil && *claudeReq.Stream

	// 路由 + 健康过滤（与 proxy 相同策略）
	upstreams, _, err := h.router.Route(model)
	if err != nil {
		writeClaudeError(rec, http.StatusNotFound, fmt.Sprintf("no upstream found for model %q", model))
		return
	}
	candidates := h.selectCandidates(upstreams, model)

	for i, up := range candidates {
		handled, retryable, ferr, _, _ := h.forwardOnce(rec, r, &claudeReq, up, model, stream, i == len(candidates)-1)
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
	writeClaudeError(rec, http.StatusBadGateway, "all upstreams failed")
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

// forwardOnce 向单个上游转发一次 Anthropic 请求（内部转 OpenAI 协议）。
// promptTokens/completionTokens 暂不解析（Anthropic 转换链路），恒为 0。
func (h *Handler) forwardOnce(w http.ResponseWriter, r *http.Request, claudeReq *ClaudeRequest, up *config.Upstream, model string, stream bool, last bool) (handled, retryable bool, err error, promptTokens, completionTokens int64) {
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
			Endpoint:   "claude",
			Status:     status,
			DurationMS: time.Since(start).Milliseconds(),
			Tokens:     0,
			APIKey:     h.recordAPIKey(r),
		})
	}()
	// 模型映射（model_mapping 只改 model 字段）
	upModel := h.router.MapModel(up, model)
	reqCopy := *claudeReq
	reqCopy.Model = upModel

	// 请求转换：Anthropic → OpenAI
	reqBody, err := ClaudeRequestToOpenAI(&reqCopy)
	if err != nil {
		writeClaudeError(w, http.StatusInternalServerError, "failed to convert request: "+err.Error())
		return true, false, nil, 0, 0
	}

	target := strings.TrimRight(up.BaseURL, "/")
	// 如果 base_url 末尾已有 /v1，不重复添加（与 proxy.forwardOnce 同样处理）
	if !strings.HasSuffix(target, "/v1") {
		target += chatPath
	} else {
		target += "/chat/completions"
	}
	reqCtx := r.Context()
	if !stream {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(reqCtx, h.timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target, bytes.NewReader(reqBody))
	if err != nil {
		if last {
			writeClaudeError(w, http.StatusInternalServerError, "failed to build upstream request")
			return true, false, nil, 0, 0
		}
		return false, true, fmt.Errorf("build upstream request: %w", err), 0, 0
	}
	req.Header.Set("Authorization", "Bearer "+up.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		if last {
			writeClaudeError(w, http.StatusBadGateway, "upstream request failed: "+err.Error())
			return true, false, nil, 0, 0
		}
		return false, true, fmt.Errorf("upstream request failed: %w", err), 0, 0
	}
	defer resp.Body.Close()

	switch {
	case stream && resp.StatusCode >= 200 && resp.StatusCode < 300:
		// 流式：上游 OpenAI SSE → Anthropic SSE 事件
		h.streamConvert(w, resp)
		return true, false, nil, 0, 0
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		if last {
			h.writeUpstreamClaudeError(w, resp)
			return true, false, fmt.Errorf("upstream error: %s", resp.Status), 0, 0
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		return false, true, fmt.Errorf("upstream error: %s", resp.Status), 0, 0
	case resp.StatusCode >= 400:
		h.writeUpstreamClaudeError(w, resp)
		return true, false, nil, 0, 0
	default:
		// 非流式：OpenAI 响应 → Anthropic 响应
		data, rerr := io.ReadAll(resp.Body)
		if rerr != nil {
			writeClaudeError(w, http.StatusBadGateway, "failed to read upstream response")
			return true, false, nil, 0, 0
		}
		claudeResp, cerr := OpenAIResponseToClaude(data, model)
		if cerr != nil {
			writeClaudeError(w, http.StatusBadGateway, "failed to convert response: "+cerr.Error())
			return true, false, nil, 0, 0
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(claudeResp)
		return true, false, nil, 0, 0
	}
}

// streamConvert 把上游 OpenAI SSE 转换为 Anthropic SSE 事件流写回客户端。
func (h *Handler) streamConvert(w http.ResponseWriter, resp *http.Response) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	if err := OpenAISSEToClaudeSSE(scanner, w); err != nil {
		h.log.Debug("stream convert", "error", err)
	}
}

// writeUpstreamClaudeError 把上游错误响应转为 Anthropic 错误格式。
func (h *Handler) writeUpstreamClaudeError(w http.ResponseWriter, resp *http.Response) {
	message := ""
	if data, err := io.ReadAll(resp.Body); err == nil {
		var openAIErr struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &openAIErr) == nil && openAIErr.Error.Message != "" {
			message = openAIErr.Error.Message
		}
	}
	if message == "" {
		message = "upstream error: " + resp.Status
	}
	status := resp.StatusCode
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	writeClaudeError(w, status, message)
}

// writeClaudeError 以 Anthropic 错误格式写响应。
func writeClaudeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(ClaudeError(status, msg))
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
