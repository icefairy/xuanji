// Package systemone 实现 SystemOne（Jev）决策协议入口：POST /v1/systemone，
// 一次请求把一段 state 交给决策模型，按 choice/noul/score 三类问题各自返回概率分布。
//
// 与 chat 链路的差异：决策模型不生成文本、不流式，响应体就是最终答案（含 usage），
// 因此这里只做「路由 + 透传 + 失败转移 + 记录」，不做任何协议内容改写。
//
// 路径说明：协议原文把端点定义为 /v1/decisions 与 /v1/systemone 互为别名，
// 但实测书生（intern-decision-4b）只提供 /v1/systemone，故转发路径固定为后者，
// 两个对外入口共用本实现。
package systemone

import (
	"bytes"
	"context"

	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/httputil"
	"github.com/icefairy/xuanji/internal/router"
	"github.com/icefairy/xuanji/internal/store"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Endpoint 是请求日志里记录的本入口端点标识。
const Endpoint = "decisions"

// decisionPath 是上游决策端点。决策模型只认这个路径（/v1/decisions 多数实现不提供）。
const decisionPath = "/v1/systemone"

// maxBodyBytes 是读取上游响应体的上限，与其它入口保持一致。
const maxBodyBytes = httputil.MaxBodyBytes

// Handler 处理 POST /v1/systemone（及别名 /v1/decisions）的转发逻辑。
type Handler struct {
	cfg      *config.Config
	router   *router.Router
	client   *http.Client
	log      *slog.Logger
	recorder RecordSink                   // 指标记录器；nil 时跳过记录
	keyName  func(r *http.Request) string // 下游 API Key 展示名解析；nil 时记空
}

// RecordSink 是请求记录接收方（生产实现为 *store.Recorder，测试可注入桩）。
type RecordSink interface {
	Record(store.Record)
}

// New 构造 handler。client 传 nil 时使用默认超时的标准 client（测试可注入）。
func New(cfg *config.Config, rt *router.Router, client *http.Client) *Handler {
	if client == nil {
		transport := config.NewForwardTransport(60 * time.Second)
		client = config.NewClientWithPiFingerprint(transport)
	}
	return &Handler{cfg: cfg, router: rt, client: client, log: slog.Default()}
}

// SetRecorder 注入指标记录器（nil 安全）。
func (h *Handler) SetRecorder(r RecordSink) { h.recorder = r }

// SetKeyName 注入下游 API Key 展示名解析器（nil 安全）。
func (h *Handler) SetKeyName(fn func(r *http.Request) string) { h.keyName = fn }

// Serve 处理一次 SystemOne 决策请求。
func (h *Handler) Serve(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var model, upstream string
	rec := &statusRecorder{ResponseWriter: w}
	defer func() {
		h.log.Info("systemone",
			"model", model, "upstream", upstream,
			"status", rec.status, "duration", time.Since(start).String())
	}()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(rec, http.StatusBadRequest, "failed to read request body", "invalid_request_error", "invalid_request")
		return
	}
	model = gjson.GetBytes(body, "model").String()
	if model == "" {
		writeError(rec, http.StatusBadRequest, "model is required", "invalid_request_error", "missing_model")
		return
	}
	// noul 问题缺 criteria 时补齐：部分上游（书生）强制校验该字段，缺了直接 400。
	// 协议规定 criteria 可选，这里按标准语义补 true/false 默认描述。
	body = fillNoulCriteria(body)

	candidates, _, rerr := h.router.Route(model)
	if rerr != nil || len(candidates) == 0 {
		writeError(rec, http.StatusNotFound,
			fmt.Sprintf("no upstream found for model %q", model), "invalid_request_error", "model_not_found")
		return
	}

	var lastErr error
	for _, up := range candidates {
		// 客户端已断开：不再尝试，也不记为上游故障
		if r.Context().Err() != nil {
			return
		}
		handled, retryable, ferr, _ := h.forward(rec, r, body, up, model)
		if handled {
			upstream = up.Name
			return
		}
		lastErr = ferr
		h.log.Warn("systemone upstream failed, trying next",
			"upstream", up.Name, "model", model, "error", ferr)
		if !retryable {
			break
		}
	}
	msg := "all upstreams failed"
	if lastErr != nil {
		msg = lastErr.Error()
	}
	writeError(rec, http.StatusBadGateway, msg, "server_error", "upstream_unreachable")
}

// forward 向单个上游转发一次决策请求。返回值语义与 proxy 其它入口一致：
//   - handled：已经向客户端写过响应（成功、透传 4xx、或错误已落盘），调用方不再重试
//   - retryable：失败是否值得换下一个候选上游
func (h *Handler) forward(w http.ResponseWriter, r *http.Request, body []byte, up *config.Upstream, model string) (
	handled, retryable bool, err error, detail string) {
	start := time.Now()
	var status int
	var promptTokens, completionTokens int64
	var recModel string
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
			UpstreamModel:    recModel,
			Endpoint:         Endpoint,
			Status:           status,
			DurationMS:       time.Since(start).Milliseconds(),
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			Tokens:           promptTokens + completionTokens,
			APIKey:           h.keyNameOf(r),
			ClientAddr:       r.RemoteAddr,
			UserAgent:        r.UserAgent(),
			ErrorDetail:      detail,
		})
	}()

	reqBody := body
	upstreamModel := h.router.MapModel(up, model)
	if upstreamModel == "" {
		upstreamModel = model
	}
	recModel = upstreamModel
	if upstreamModel != model {
		if reqBody, err = sjson.SetBytes(body, "model", upstreamModel); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to rewrite model field", "server_error", "")
			return true, false, nil, ""
		}
	}

	target := upstreamURL(up.BaseURL)
	// 超时由 client 的 Timeout 统一控制（New 里按全局配置注入）；这里只继承客户端取消信号。
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(reqBody))
	if err != nil {
		return false, true, fmt.Errorf("build systemone request: %w", err), ""
	}
	setAuth(req, up)
	req.Header.Set("Content-Type", "application/json")
	config.ApplyUpstreamUserAgent(req)

	resp, err := h.client.Do(req)
	if err != nil {
		// 客户端断连不是上游故障
		if errors.Is(err, context.Canceled) {
			return true, false, err, ""
		}
		return false, true, fmt.Errorf("systemone upstream request failed: %w", err), err.Error()
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		data, _ := httputil.ReadBody(resp.Body)
		detail = summarizeError(data, resp.Status)
		// 4xx = 客户端请求本身有问题（问题字段非法等）：透传，不换上游重试
		if resp.StatusCode < 500 {
			copyHeader(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(data)
			return true, false, nil, detail
		}
		return false, true, fmt.Errorf("systemone upstream error: %s", resp.Status), detail
	}

	data, readErr := httputil.ReadBody(resp.Body)
	if readErr != nil {
		return false, true, fmt.Errorf("read systemone response: %w", readErr), readErr.Error()
	}
	// usage 字段与 OpenAI 不同：input_tokens / output_tokens
	usage := gjson.GetBytes(data, "usage")
	promptTokens = usage.Get("input_tokens").Int()
	completionTokens = usage.Get("output_tokens").Int()
	if promptTokens == 0 && completionTokens == 0 {
		promptTokens = usage.Get("prompt_tokens").Int()
		completionTokens = usage.Get("completion_tokens").Int()
	}

	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(data)
	return true, false, nil, ""
}

// fillNoulCriteria 给缺少 criteria 的 noul 问题补上 true/false 默认描述。
// 已有 criteria（哪怕只有一半）保持原样，不覆盖客户端的显式配置。
func fillNoulCriteria(body []byte) []byte {
	questions := gjson.GetBytes(body, "questions")
	if !questions.Exists() || !questions.IsObject() {
		return body
	}
	out := body
	var setErr error
	questions.ForEach(func(key, val gjson.Result) bool {
		if val.Get("type").String() != "noul" {
			return true
		}
		if crit := val.Get("criteria"); crit.Exists() && crit.IsObject() {
			return true
		}
		path := "questions." + key.String() + ".criteria"
		if out, setErr = sjson.SetBytes(out, path, map[string]string{"true": "true", "false": "false"}); setErr != nil {
			return false
		}
		return true
	})
	if setErr != nil {
		return body
	}
	return out
}

// upstreamURL 拼接决策端点。上游 base_url 两种写法都存在（带或不带 /v1），
// 已以 /v1 结尾时不再重复拼接，避免出现 /v1/v1/systemone。
func upstreamURL(baseURL string) string {
	base := strings.TrimRight(baseURL, "/")
	if strings.HasSuffix(base, "/v1") {
		base = strings.TrimSuffix(base, "/v1")
	}
	return base + decisionPath
}

// upstreamTimeout 返回单个上游的请求超时：上游自有 timeout 优先，否则跟随全局设置。
func upstreamTimeout(up *config.Upstream, cfg *config.Config) time.Duration {
	if up != nil && up.Timeout > 0 {
		return time.Duration(up.Timeout) * time.Second
	}
	if cfg != nil && cfg.Retry.UpstreamTimeout > 0 {
		return time.Duration(cfg.Retry.UpstreamTimeout) * time.Second
	}
	return 60 * time.Second
}

// setAuth 设置上游鉴权头。决策上游目前均为标准 Bearer；Dots 风格保留兼容。
func setAuth(req *http.Request, up *config.Upstream) {
	if up.IsDots() {
		req.Header.Set("api-key", up.APIKey)
		req.Header.Del("Authorization")
		return
	}
	req.Header.Set("Authorization", "Bearer "+up.APIKey)
}

func (h *Handler) keyNameOf(r *http.Request) string {
	if h.keyName == nil {
		return ""
	}
	return h.keyName(r)
}

func summarizeError(data []byte, status string) string {
	if msg := gjson.GetBytes(data, "error.message").String(); msg != "" {
		return msg
	}
	if len(data) > 0 {
		s := string(data)
		if len(s) > 200 {
			s = s[:200] + "...(truncated)"
		}
		return s
	}
	return status
}

func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// statusRecorder 记录写出的 HTTP 状态码，供请求日志使用。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// apiError 是 OpenAI 风格的错误响应体。
type apiError struct {
	Error apiErrorDetail `json:"error"`
}

type apiErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

func writeError(w http.ResponseWriter, status int, message, typ, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiError{Error: apiErrorDetail{Message: message, Type: typ, Code: code}})
}
