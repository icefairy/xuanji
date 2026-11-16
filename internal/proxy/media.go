package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/store"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	mediaImagePath               = "/images/generations"
	mediaAudioSpeechPath         = "/audio/speech"
	mediaAudioTranscriptionsPath = "/audio/transcriptions"
)

// ImageGenerations 处理 POST /v1/images/generations。
func (h *Handler) ImageGenerations(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w}
	start := time.Now()

	var model, upstream string

	defer func() {
		h.log.Info("images/generations",
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
	model = gjson.GetBytes(body, "model").String()
	if model == "" {
		writeError(rec, http.StatusBadRequest, "model is required", "invalid_request_error", "missing_model")
		return
	}

	upstreams, strategy, err := h.router.Route(model)
	if err != nil {
		writeError(rec, http.StatusNotFound, fmt.Sprintf("no upstream found for model %q", model), "invalid_request_error", "model_not_found")
		return
	}

	candidates := h.selectCandidates(upstreams, strategy, model)
	for i, up := range candidates {
		handled, retryable, ferr := h.forwardMediaJSON(rec, r, r.Context(), body, up, model, mediaImagePath, "images", i == len(candidates)-1)
		if h.shouldMarkUpstreamFailure(handled, ferr) {
			h.health.MarkFailure(up.Name)
		}
		if handled {
			upstream = up.Name
			return
		}
		h.log.Warn("upstream failed, trying next",
			"upstream", up.Name, "model", model, "error", ferr)
		if !retryable {
			break
		}
	}
	writeError(rec, http.StatusBadGateway, "all upstreams failed", "server_error", "upstream_unreachable")
}

// AudioSpeech 处理 POST /v1/audio/speech。
func (h *Handler) AudioSpeech(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w}
	start := time.Now()

	var model, upstream string

	defer func() {
		h.log.Info("audio/speech",
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
	model = gjson.GetBytes(body, "model").String()
	if model == "" {
		writeError(rec, http.StatusBadRequest, "model is required", "invalid_request_error", "missing_model")
		return
	}

	upstreams, strategy, err := h.router.Route(model)
	if err != nil {
		writeError(rec, http.StatusNotFound, fmt.Sprintf("no upstream found for model %q", model), "invalid_request_error", "model_not_found")
		return
	}

	candidates := h.selectCandidates(upstreams, strategy, model)
	for i, up := range candidates {
		var handled, retryable bool
		var ferr error
		if strings.HasPrefix(model, "mimo-v2.5-tts") && !isLocalTTSUpstream(up) {
			handled, retryable, ferr = h.forwardMimoTTS(rec, r.Context(), body, up, model, i == len(candidates)-1)
		} else if isUnisoundAsyncModel(model) && isUnisoundUpstream(up) {
			// 云知声 U2-TTS 走异步任务协议（/audio/speech/tasks），
			// 上游没有 OpenAI 的 /audio/speech 端点，直接转发必然 404
			handled, retryable, ferr = h.forwardUnisoundTTS(rec, r.Context(), body, up, model, i == len(candidates)-1)
		} else {
			handled, retryable, ferr = h.forwardMediaJSON(rec, r, r.Context(), body, up, model, mediaAudioSpeechPath, "audio", i == len(candidates)-1)
		}
		if h.shouldMarkUpstreamFailure(handled, ferr) {
			h.health.MarkFailure(up.Name)
		}
		if handled {
			upstream = up.Name
			return
		}
		h.log.Warn("upstream failed, trying next",
			"upstream", up.Name, "model", model, "error", ferr)
		if !retryable {
			break
		}
	}
	writeError(rec, http.StatusBadGateway, "all upstreams failed", "server_error", "upstream_unreachable")
}

// AudioTranscriptions 处理 POST /v1/audio/transcriptions。
func (h *Handler) AudioTranscriptions(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w}
	start := time.Now()

	var model, upstream string

	defer func() {
		h.log.Info("audio/transcriptions",
			"method", r.Method,
			"path", r.URL.Path,
			"model", model,
			"upstream", upstream,
			"status", rec.status,
			"duration", time.Since(start).String(),
		)
	}()

	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(rec, http.StatusBadRequest, "failed to read request body", "invalid_request_error", "invalid_request")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(rawBody))

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(rec, http.StatusBadRequest, "failed to parse multipart form: "+err.Error(), "invalid_request_error", "parse_error")
		return
	}
	model = r.FormValue("model")
	if model == "" {
		writeError(rec, http.StatusBadRequest, "model is required", "invalid_request_error", "missing_model")
		return
	}

	upstreams, strategy, err := h.router.Route(model)
	if err != nil {
		writeError(rec, http.StatusNotFound, fmt.Sprintf("no upstream found for model %q", model), "invalid_request_error", "model_not_found")
		return
	}

	candidates := h.selectCandidates(upstreams, strategy, model)
	for i, up := range candidates {
		handled, retryable, ferr := h.forwardAudioTranscription(rec, r, rawBody, up, model, "audio", i == len(candidates)-1)
		if h.shouldMarkUpstreamFailure(handled, ferr) {
			h.health.MarkFailure(up.Name)
		}
		if handled {
			upstream = up.Name
			return
		}
		h.log.Warn("upstream failed, trying next",
			"upstream", up.Name, "model", model, "error", ferr)
		if !retryable {
			break
		}
	}
	writeError(rec, http.StatusBadGateway, "all upstreams failed", "server_error", "upstream_unreachable")
}

// forwardMediaJSON 向上游转发 JSON 请求体，响应原样透传（JSON 或二进制均可）。
// 用于 ImageGenerations 和 AudioSpeech 这类 JSON 请求端点。
// endpoint 是记录用的端点名（"images" / "audio"）。
func (h *Handler) forwardMediaJSON(w http.ResponseWriter, r *http.Request, ctx context.Context, body []byte, up *config.Upstream, model, pathSuffix, endpoint string, last bool) (handled, retryable bool, err error) {
	start := time.Now()
	var status int
	// errorDetail 记录上游错误体摘要，由下方错误分支写入，defer 统一落库。
	// 与 chat/rerank/embeddings 链路对齐：失败必须留下可定位的原因（2026-10-18 修复）。
	var errorDetail string
	mapped := h.router.MapModel(up, model) // 上游真实模型名，供 defer 记录
	defer func() {
		if h.recorder == nil || !handled {
			return
		}
		if sr, ok := w.(*statusRecorder); ok {
			status = sr.status
		}
		h.recorder.Record(store.Record{
			Timestamp:     time.Now(),
			Upstream:      up.Name,
			Model:         model,
			UpstreamModel: mapped,
			Endpoint:      endpoint,
			Status:        status,
			DurationMS:    time.Since(start).Milliseconds(),
			Tokens:        0,
			APIKey:        h.recordAPIKey(r),
			ClientAddr:    r.RemoteAddr,  // 客户端地址 "IP:port"，用于区分调用程序
			UserAgent:     r.UserAgent(), // 客户端 UA，程序识别最强信号
			ErrorDetail:   errorDetail,
		})
	}()
	reqBody := body
	if mapped != model {
		if reqBody, err = sjson.SetBytes(body, "model", mapped); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to rewrite model field", "server_error", "")
			return true, false, nil
		}
	}

	target := strings.TrimRight(up.BaseURL, "/") + pathSuffix
	reqCtx, cancel := context.WithTimeout(ctx, upstreamTimeoutForUp(up, h.cfg))
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target, bytes.NewReader(reqBody))
	if err != nil {
		if last {
			writeError(w, http.StatusInternalServerError, "failed to build upstream request", "server_error", "")
			return true, false, nil
		}
		return false, true, fmt.Errorf("build upstream request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+up.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		if last {
			writeError(w, http.StatusBadGateway, upstreamErrorMessage(err), "server_error", "upstream_unreachable")
			return true, false, nil
		}
		return false, true, fmt.Errorf("upstream request failed: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		if last {
			errorDetail = h.consumeUpstreamErrorDetail(resp, up, model, model, body)
			h.writeUpstreamError(w, resp)
			return true, false, fmt.Errorf("upstream error: %s", resp.Status)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		return false, true, fmt.Errorf("upstream error: %s", resp.Status)
	case resp.StatusCode >= 400:
		errorDetail = h.consumeUpstreamErrorDetail(resp, up, model, model, body)
		// 模型不存在/无权限（404 model_not_found 等）：该上游没有这个模型，
		// 但**下一个候选上游可能有**（典型：stepaudio-2.5-tts → [stepfun, piper-local]）。
		// 修复前一律 return true,false 直接透传：handled=true 使候选循环 return，
		// 备用上游从未被尝试。
		//
		// 2026-11-06 实测：stepfun 回 404 "The model \"stepaudio-2.5-tts\" does not exist
		// or you do not have access to it"，路由明明配了 piper-local 备用，
		// 11-05 单日 156 次 404 直接透传给客户端，piper-local 零次承接。
		//
		// 与 chat / rerank 链路对齐：命中时返回 retryable=true（换上游可能成功），
		// 但不计入上游故障（模型不存在不是上游不健康）。
		// consumeUpstreamErrorDetail 已把 resp.Body 还原，这里可重读全文。
		if !last && isModelUnavailableError(resp.StatusCode, resp.Body) {
			h.log.Info("media upstream has no such model, trying next",
				"upstream", up.Name, "model", model, "status", resp.StatusCode)
			return false, true, &clientRequestError{retryableUpstreamError("upstream error", resp, errorDetail)}
		}
		h.writeUpstreamError(w, resp)
		return true, false, nil
	default:
		copyHeader(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		if _, err := io.Copy(w, resp.Body); err != nil {
			h.log.Debug("copy upstream body", "error", err)
		}
		return true, false, nil
	}
}

// isModelUnavailableError 判断一次上游 4xx 响应是否属于「**该上游**没有这个模型/无权限」。
//
// 与 isClientRequestError 的区别：后者回答「是不是客户端请求内容问题」，用于决定
// **要不要计入上游健康**（模型不存在 → 不计）；本函数回答「换个上游会不会好」，
// 用于决定 media 链路**要不要继续尝试下一个候选**。
//
// 为什么需要单独一个函数：media 链路（TTS/ASR/图像）遇到 4xx 原先是直接透传
// （return true,false = handled 且不重试），而 chat/rerank 链路是 `false,true`
// （换上游可能成功）。同一个「模型不存在」在上游 A 失败、在上游 B 却可能成功，
// 因为每家提供的模型目录不同。
//
// 2026-11-06 实测：stepaudio-2.5-tts 路由配了 [stepfun, piper-local]，
// stepfun 回 404 "The model ... does not exist or you do not have access to it"，
// 因 retryable=false 导致候选循环 break，本地 piper 兼底从未被尝试，
// 11-05 单日 156 次 404 直接透传给客户端。
//
// 注意：**只应包含“该上游没有这个东西”的措辞**。参数错误类（如 input 为空、
// voice 不存在）换哪个上游都一样失败，不属本函数范围，否则会放大延迟与上游压力。
func isModelUnavailableError(status int, body io.Reader) bool {
	switch status {
	case http.StatusNotFound, http.StatusBadRequest, http.StatusForbidden, http.StatusMethodNotAllowed:
	default:
		return false
	}
	if body == nil {
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(body, 64<<10))
	if err != nil {
		return false
	}
	msg := strings.ToLower(string(raw))
	for _, kw := range modelUnavailableReasons {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

// modelUnavailableReasons 是「该上游不提供此模型/无权限」的响应特征（小写子串匹配）。
//
// 只放**换上游可能改变结果**的措辞；参数类错误（input/voice/图片尺寸等）不在此列。
var modelUnavailableReasons = []string{
	// stepfun TTS（2026-11-06 实测）：
	//   The model "stepaudio-2.5-tts" does not exist or you do not have access to it.
	"does not exist",
	"do not have access",
	// 通用模型不存在措辞（与 clientErrReasons 中的同名项保持口径一致）
	"model not found",
	"model_not_found",
	"no such model",
	// 上游无此端点（如把 OpenAI 的 /audio/speech 打到不支持它的上游）
	"model route not found",
}

// consumeUpstreamErrorDetail 记录上游错误详情到日志，并返回错误体摘要供落库。
// 复用 consumeUpstreamError 的读取/还原逻辑，只是额外把摘要返回给调用方写 request_log。
func (h *Handler) consumeUpstreamErrorDetail(resp *http.Response, up *config.Upstream, model, upstreamModel string, reqBody []byte) string {
	respBody, _ := readUpstreamBody(resp.Body)
	resp.Body = io.NopCloser(bytes.NewReader(respBody))
	LogUpstreamErrorDetail(h.log, up, model, upstreamModel, resp.StatusCode, reqBody, respBody)
	if len(respBody) == 0 {
		return ""
	}
	return summarizeUpstreamError(respBody)
}

// forwardAudioTranscription 向上游转发 multipart 音频转录请求。
// endpoint 是记录用的端点名（"audio"）。
func (h *Handler) forwardAudioTranscription(w http.ResponseWriter, r *http.Request, rawBody []byte, up *config.Upstream, model, endpoint string, last bool) (handled, retryable bool, err error) {
	start := time.Now()
	var status int
	mapped := h.router.MapModel(up, model) // 上游真实模型名，供 defer 记录
	defer func() {
		if h.recorder == nil || !handled {
			return
		}
		if sr, ok := w.(*statusRecorder); ok {
			status = sr.status
		}
		h.recorder.Record(store.Record{
			Timestamp:     time.Now(),
			Upstream:      up.Name,
			Model:         model,
			UpstreamModel: mapped,
			Endpoint:      endpoint,
			Status:        status,
			DurationMS:    time.Since(start).Milliseconds(),
			Tokens:        0,
			APIKey:        h.recordAPIKey(r),
			ClientAddr:    r.RemoteAddr,  // 客户端地址 "IP:port"，用于区分调用程序
			UserAgent:     r.UserAgent(), // 客户端 UA，程序识别最强信号
		})
	}()
	var reqBody io.Reader
	contentType := r.Header.Get("Content-Type")

	if mapped != model {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)

		file, header, ferr := r.FormFile("file")
		if ferr != nil {
			if last {
				writeError(w, http.StatusBadRequest, "failed to read file from multipart form: "+ferr.Error(), "invalid_request_error", "")
				return true, false, nil
			}
			return false, true, fmt.Errorf("read file from multipart: %w", ferr)
		}
		fw, cerr := mw.CreateFormFile("file", header.Filename)
		if cerr != nil {
			file.Close()
			if last {
				writeError(w, http.StatusInternalServerError, "failed to create form file: "+cerr.Error(), "server_error", "")
				return true, false, nil
			}
			return false, true, fmt.Errorf("create form file: %w", cerr)
		}
		if _, cerr := io.Copy(fw, file); cerr != nil {
			file.Close()
			if last {
				writeError(w, http.StatusInternalServerError, "failed to copy file: "+cerr.Error(), "server_error", "")
				return true, false, nil
			}
			return false, true, fmt.Errorf("copy file: %w", cerr)
		}
		file.Close()

		if cerr := mw.WriteField("model", mapped); cerr != nil {
			if last {
				writeError(w, http.StatusInternalServerError, "failed to write model field: "+cerr.Error(), "server_error", "")
				return true, false, nil
			}
			return false, true, fmt.Errorf("write model field: %w", cerr)
		}
		for key, values := range r.MultipartForm.Value {
			if key != "model" {
				for _, v := range values {
					mw.WriteField(key, v)
				}
			}
		}
		mw.Close()

		reqBody = &buf
		contentType = mw.FormDataContentType()
	} else {
		reqBody = bytes.NewReader(rawBody)
	}

	target := strings.TrimRight(up.BaseURL, "/") + mediaAudioTranscriptionsPath
	reqCtx, cancel := context.WithTimeout(r.Context(), upstreamTimeoutForUp(up, h.cfg))
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target, reqBody)
	if err != nil {
		if last {
			writeError(w, http.StatusInternalServerError, "failed to build upstream request", "server_error", "")
			return true, false, nil
		}
		return false, true, fmt.Errorf("build upstream request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+up.APIKey)
	req.Header.Set("Content-Type", contentType)

	resp, err := h.client.Do(req)
	if err != nil {
		if last {
			writeError(w, http.StatusBadGateway, upstreamErrorMessage(err), "server_error", "upstream_unreachable")
			return true, false, nil
		}
		return false, true, fmt.Errorf("upstream request failed: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		if last {
			h.consumeUpstreamError(resp, up, model, model, rawBody)
			h.writeUpstreamError(w, resp)
			return true, false, fmt.Errorf("upstream error: %s", resp.Status)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		return false, true, fmt.Errorf("upstream error: %s", resp.Status)
	case resp.StatusCode >= 400:
		h.consumeUpstreamError(resp, up, model, model, rawBody)
		// 与 forwardMediaJSON 同款修复：该上游没这个模型/无权限时，
		// 下一个候选上游可能有（语音转写同样存在多厂商模型目录差异）。
		// 2026-11-02 实测 unisound 把 u2-asr 发到 /chat/completions 回 404
		// "model route not found"，6 次全部透传给客户端，从未尝试备用。
		if !last && isModelUnavailableError(resp.StatusCode, resp.Body) {
			h.log.Info("transcription upstream has no such model, trying next",
				"upstream", up.Name, "model", model, "status", resp.StatusCode)
			return false, true, nil
		}
		h.writeUpstreamError(w, resp)
		return true, false, nil
	default:
		copyHeader(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		if _, err := io.Copy(w, resp.Body); err != nil {
			h.log.Debug("copy upstream body", "error", err)
		}
		return true, false, nil
	}
}

// forwardMimoTTS 将标准 OpenAI /v1/audio/speech 请求转换为小米 MiMo 的
// /v1/chat/completions + audio 字段格式，并把响应中 base64 音频解码后返回。
// MiMo 的 TTS 不走标准 /audio/speech 端点，且不能有 system 角色、
// 必须同时含 user 和 assistant 角色，鉴权头用 api-key 而非 Bearer。
func (h *Handler) forwardMimoTTS(w http.ResponseWriter, ctx context.Context, body []byte, up *config.Upstream, model string, last bool) (handled, retryable bool, err error) {
	input := gjson.GetBytes(body, "input").String()
	voice := gjson.GetBytes(body, "voice").String()
	if voice == "" {
		voice = "mimo_default"
	}
	format := gjson.GetBytes(body, "response_format").String()
	if format == "" {
		format = "mp3"
	}

	payload := map[string]any{
		"model": h.router.MapModel(up, model),
		"messages": []map[string]string{
			{"role": "user", "content": input},
			{"role": "assistant", "content": ""},
		},
		"audio": map[string]string{
			"voice":  mapMimoVoice(voice),
			"format": format,
		},
	}
	reqBody, err := json.Marshal(payload)
	if err != nil {
		if last {
			writeError(w, http.StatusInternalServerError, "failed to build mimo request body: "+err.Error(), "server_error", "")
			return true, false, nil
		}
		return false, true, fmt.Errorf("build mimo request body: %w", err)
	}

	// MiMo TTS 端点：chat/completions（而非 /audio/speech），鉴权头 api-key
	target := strings.TrimRight(up.BaseURL, "/") + "/chat/completions"
	reqCtx, cancel := context.WithTimeout(ctx, upstreamTimeoutForUp(up, h.cfg))
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target, bytes.NewReader(reqBody))
	if err != nil {
		if last {
			writeError(w, http.StatusInternalServerError, "failed to build upstream request", "server_error", "")
			return true, false, nil
		}
		return false, true, fmt.Errorf("build upstream request: %w", err)
	}
	req.Header.Set("api-key", up.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		if last {
			writeError(w, http.StatusBadGateway, upstreamErrorMessage(err), "server_error", "upstream_unreachable")
			return true, false, nil
		}
		return false, true, fmt.Errorf("upstream request failed: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		if last {
			h.consumeUpstreamError(resp, up, model, model, body)
			h.writeUpstreamError(w, resp)
			return true, false, fmt.Errorf("upstream error: %s", resp.Status)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		return false, true, fmt.Errorf("upstream error: %s", resp.Status)
	case resp.StatusCode >= 400:
		h.consumeUpstreamError(resp, up, model, model, body)
		h.writeUpstreamError(w, resp)
		return true, false, nil
	default:
		respBody, rerr := readUpstreamBody(resp.Body)
		if rerr != nil {
			if last {
				writeError(w, http.StatusBadGateway, "failed to read upstream response: "+rerr.Error(), "server_error", "upstream_unreachable")
				return true, false, nil
			}
			return false, true, fmt.Errorf("read upstream response: %w", rerr)
		}
		data := gjson.GetBytes(respBody, "choices.0.message.audio.data").String()
		audio, derr := base64.StdEncoding.DecodeString(data)
		if derr != nil {
			writeError(w, http.StatusBadGateway, "failed to decode mimo audio", "server_error", "")
			return true, false, nil
		}
		// 原响应是 JSON，必须转成音频二进制返回
		w.Header().Set("Content-Type", "audio/"+format)
		w.WriteHeader(http.StatusOK)
		if _, werr := w.Write(audio); werr != nil {
			h.log.Debug("write mimo audio", "error", werr)
		}
		return true, false, nil
	}
}

// mapMimoVoice 将 OpenAI 标准音色映射为 MiMo 音色；其余音色原样透传。
func mapMimoVoice(voice string) string {
	switch voice {
	case "alloy", "echo", "fable", "onyx", "nova", "shimmer":
		return "mimo_default"
	default:
		return voice
	}
}
