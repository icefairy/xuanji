// Package ollama 实现 Ollama 原生协议上游支持。
// 提供 /api/chat、/api/generate、/api/embed 三个原生入口（透传），
// 以及 OpenAI 兼容入口到 ollama 上游的协议转换（/v1/chat/completions、/v1/embeddings）。
package ollama

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
const upstreamTimeout = 60 * time.Second

// Handler 处理 Ollama 原生协议请求。
type Handler struct {
	router   *router.Router
	health   *health.Checker
	client   *http.Client
	log      *slog.Logger
	recorder *store.Recorder              // 指标记录器；nil 时跳过记录
	timeout  time.Duration                // 上游请求超时；默认 60s，可用 SetTimeout 覆盖
	keyName  func(r *http.Request) string // 下游 API Key 展示名（统计用）；nil 时记录空
}

// SetTimeout 设置上游请求超时（来自 retry.upstream_timeout 配置）。
func (h *Handler) SetTimeout(d time.Duration) {
	if d > 0 {
		h.timeout = d
	}
}

// New 创建 Ollama 协议 Handler。
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

// SetRecorder 注入指标记录器（nil 安全；测试不需要调用）。
func (h *Handler) SetRecorder(r *store.Recorder) {
	h.recorder = r
}

// Chat 处理 POST /api/chat（Ollama 原生聊天接口，透传）。
func (h *Handler) Chat(w http.ResponseWriter, r *http.Request) {
	h.forward(w, r, "/api/chat")
}

// Generate 处理 POST /api/generate（Ollama 原生生成接口，透传）。
func (h *Handler) Generate(w http.ResponseWriter, r *http.Request) {
	h.forward(w, r, "/api/generate")
}

// Embed 处理 POST /api/embed（Ollama 原生嵌入接口，透传）。
func (h *Handler) Embed(w http.ResponseWriter, r *http.Request) {
	h.forward(w, r, "/api/embed")
}

// OpenAICompletions 处理 OpenAI /v1/chat/completions 路由到 ollama 上游的转换。
func (h *Handler) OpenAICompletions(w http.ResponseWriter, r *http.Request) {
	h.openAIForward(w, r, "chat")
}

// OpenAIEmbeddings 处理 OpenAI /v1/embeddings 路由到 ollama 上游的转换。
func (h *Handler) OpenAIEmbeddings(w http.ResponseWriter, r *http.Request) {
	h.openAIForward(w, r, "embed")
}

// extractModel 从请求体中提取 model 字段。
func extractModel(body []byte) string {
	var req struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &req)
	return req.Model
}

// selectUpstream 路由 + 健康过滤，选出一个可用的 ollama 上游。
// 返回 nil 表示无可用上游。
func (h *Handler) selectUpstream(model string, w http.ResponseWriter) *config.Upstream {
	upstreams, _, err := h.router.Route(model)
	if err != nil {
		writeOllamaError(w, http.StatusNotFound, fmt.Sprintf("no upstream found for model %q", model))
		return nil
	}
	candidates := upstreams
	if h.health != nil {
		healthy := h.health.HealthyUpstreams(upstreams)
		if len(healthy) > 0 {
			candidates = healthy
		} else {
			h.log.Warn("no healthy upstream, falling back to first", "model", model)
			candidates = upstreams[:1]
		}
	}
	for _, up := range candidates {
		if up.IsOllama() {
			return up
		}
	}
	// 没有 ollama 类型的候选：取第一个（可能是 openai 兼容上游，交给转换层处理）
	return candidates[0]
}

// forward 处理 Ollama 原生入口：解析 model → 路由 → 映射模型 → 透传到 ollama 上游。
func (h *Handler) forward(w http.ResponseWriter, r *http.Request, path string) {
	start := time.Now()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOllamaError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	model := extractModel(body)
	if model == "" {
		writeOllamaError(w, http.StatusBadRequest, "model is required")
		return
	}

	up := h.selectUpstream(model, w)
	if up == nil {
		return
	}
	// 模型映射
	upModel := h.router.MapModel(up, model)
	if upModel != model {
		var m map[string]any
		if json.Unmarshal(body, &m) == nil {
			m["model"] = upModel
			body, _ = json.Marshal(m)
		}
	}

	// 判断是否流式（ollama 默认流式）
	stream := true
	var streamReq struct {
		Stream *bool `json:"stream"`
	}
	if json.Unmarshal(body, &streamReq) == nil && streamReq.Stream != nil {
		stream = *streamReq.Stream
	}

	// 透传转发
	status, streamOK, ferr := h.passthrough(w, r, up, path, body, stream)
	if ferr != nil && h.health != nil {
		h.health.MarkFailure(up.Name)
	}
	if h.recorder != nil {
		h.recorder.Record(store.Record{
			Timestamp:  time.Now(),
			Upstream:   up.Name,
			Model:      model,
			Endpoint:   ollamaEndpointForPath(path),
			Status:     status,
			DurationMS: time.Since(start).Milliseconds(),
			Tokens:     0,
			APIKey:     h.recordAPIKey(r),
			ClientAddr: r.RemoteAddr, // 客户端地址 "IP:port"，用于区分调用程序
			UserAgent:  r.UserAgent(), // 客户端 UA，程序识别最强信号
		})
	}
	h.log.Info("ollama "+path,
		"model", model,
		"upstream", up.Name,
		"stream", stream,
		"status", status,
		"stream_ok", streamOK,
		"duration", time.Since(start).String(),
		"error", ferr,
	)
}

// passthrough 把请求体原样转发到上游 path 端点。
func (h *Handler) passthrough(w http.ResponseWriter, r *http.Request, up *config.Upstream, path string, body []byte, stream bool) (status int, streamOK bool, err error) {
	target := strings.TrimRight(up.BaseURL, "/") + path
	reqCtx := r.Context()
	if !stream {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(reqCtx, h.timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		writeOllamaError(w, http.StatusInternalServerError, "failed to build upstream request")
		return 0, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if up.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+up.APIKey)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		writeOllamaError(w, http.StatusBadGateway, "upstream request failed: "+err.Error())
		return 0, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		msg := ""
		if data, rerr := io.ReadAll(resp.Body); rerr == nil {
			var e struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(data, &e) == nil && e.Error != "" {
				msg = e.Error
			}
		}
		if msg == "" {
			msg = "upstream error: " + resp.Status
		}
		writeOllamaError(w, resp.StatusCode, msg)
		return resp.StatusCode, false, fmt.Errorf("upstream error: %s", resp.Status)
	}

	// 透传响应（流式 NDJSON 或普通 JSON）
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if stream {
		// 流式：NDJSON 原样透传，需要 flush
		if f, ok := w.(http.Flusher); ok {
			defer f.Flush()
		}
	}
	_, _ = io.Copy(w, resp.Body)
	return resp.StatusCode, stream, nil
}

// openAIForward 处理 OpenAI 兼容入口路由到 ollama 上游的转换。
func (h *Handler) openAIForward(w http.ResponseWriter, r *http.Request, kind string) {
	rec := &statusRecorder{ResponseWriter: w}
	start := time.Now()

	var model string
	var up *config.Upstream
	defer func() {
		if h.recorder == nil || up == nil {
			return
		}
		h.recorder.Record(store.Record{
			Timestamp:  time.Now(),
			Upstream:   up.Name,
			Model:      model,
			Endpoint:   ollamaEndpointForKind(kind),
			Status:     rec.status,
			DurationMS: time.Since(start).Milliseconds(),
			Tokens:     0,
			ClientAddr: r.RemoteAddr, // 客户端地址 "IP:port"，用于区分调用程序
			UserAgent:  r.UserAgent(), // 客户端 UA，程序识别最强信号
		})
	}()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOllamaError(rec, http.StatusBadRequest, "failed to read request body")
		return
	}
	model = extractModel(body)
	if model == "" {
		writeOllamaError(rec, http.StatusBadRequest, "model is required")
		return
	}

	up = h.selectUpstream(model, rec)
	if up == nil {
		return
	}
	// 只处理 ollama 上游；否则交给 OpenAI proxy 处理（由 main.go 路由决定，这里防御性跳过）
	if !up.IsOllama() {
		writeOllamaError(rec, http.StatusBadGateway, "upstream is not ollama type")
		return
	}
	upModel := h.router.MapModel(up, model)

	// 判断流式
	stream := false
	var streamReq struct {
		Stream bool `json:"stream"`
	}
	if json.Unmarshal(body, &streamReq) == nil {
		stream = streamReq.Stream
	}

	var path string
	var upstreamBody []byte
	var errConv error

	switch kind {
	case "chat":
		path = "/api/chat"
		upstreamBody, errConv = OpenAIChatToOllamaChat(body)
		if errConv == nil && upModel != model {
			var m map[string]any
			if json.Unmarshal(upstreamBody, &m) == nil {
				m["model"] = upModel
				upstreamBody, _ = json.Marshal(m)
			}
		}
	case "embed":
		path = "/api/embed"
		upstreamBody, errConv = OpenAIToOllamaEmbed(body)
		if errConv == nil && upModel != model {
			var m map[string]any
			if json.Unmarshal(upstreamBody, &m) == nil {
				m["model"] = upModel
				upstreamBody, _ = json.Marshal(m)
			}
		}
	}
	if errConv != nil {
		writeOllamaError(rec, http.StatusBadRequest, "convert request: "+errConv.Error())
		return
	}

	// 转发到 ollama 上游
	target := strings.TrimRight(up.BaseURL, "/") + path
	reqCtx := r.Context()
	if !stream {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(reqCtx, h.timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target, bytes.NewReader(upstreamBody))
	if err != nil {
		writeOllamaError(rec, http.StatusInternalServerError, "failed to build upstream request")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if up.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+up.APIKey)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		if h.health != nil {
			h.health.MarkFailure(up.Name)
		}
		writeOllamaError(rec, http.StatusBadGateway, "upstream request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		msg := ""
		if data, rerr := io.ReadAll(resp.Body); rerr == nil {
			var e struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(data, &e) == nil && e.Error != "" {
				msg = e.Error
			}
		}
		if msg == "" {
			msg = "upstream error: " + resp.Status
		}
		writeOllamaError(w, resp.StatusCode, msg)
		return
	}

	switch kind {
	case "chat":
		if stream {
			// Ollama NDJSON → OpenAI SSE
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.WriteHeader(http.StatusOK)
			if err := StreamOllamaChatToOpenAI(resp.Body, w); err != nil {
				h.log.Debug("ollama chat stream convert", "error", err)
			}
		} else {
			data, rerr := io.ReadAll(resp.Body)
			if rerr != nil {
				writeOllamaError(w, http.StatusBadGateway, "read upstream response failed")
				return
			}
			out, cerr := OllamaChatResponseToOpenAI(data, model)
			if cerr != nil {
				writeOllamaError(w, http.StatusBadGateway, "convert response: "+cerr.Error())
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(out)
		}
	case "embed":
		data, rerr := io.ReadAll(resp.Body)
		if rerr != nil {
			writeOllamaError(w, http.StatusBadGateway, "read upstream response failed")
			return
		}
		out, cerr := OllamaEmbedToOpenAI(data)
		if cerr != nil {
			writeOllamaError(w, http.StatusBadGateway, "convert response: "+cerr.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
	}

	if h.health != nil {
		// 成功，重置失败计数（由健康检查负责，这里不处理）
	}
	h.log.Info("ollama openai-compat "+kind,
		"model", model,
		"upstream", up.Name,
		"stream", stream,
		"duration", time.Since(start).String(),
	)
}

// writeOllamaError 以 Ollama 错误格式写响应。
func writeOllamaError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// hopByHopHeaders 是 HTTP/1.1 逐跳头，转发时不得复制。
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// copyHeader 复制上游响应头到客户端，跳过逐跳头与 Content-Length。
func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		if hopByHopHeaders[k] {
			continue
		}
		if k == "Content-Length" {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// statusRecorder 记录实际写出的状态码，供请求日志与埋点使用。
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

// ollamaEndpointForPath 映射 ollama 原生路径到端点类型名。
func ollamaEndpointForPath(path string) string {
	switch path {
	case "/api/chat":
		return "chat"
	case "/api/generate":
		return "generate"
	case "/api/embed":
		return "embed"
	default:
		return "ollama"
	}
}

// ollamaEndpointForKind 映射 OpenAI 兼容 kind 到端点类型名。
func ollamaEndpointForKind(kind string) string {
	switch kind {
	case "chat":
		return "chat"
	case "embed":
		return "embed"
	default:
		return "ollama"
	}
}
