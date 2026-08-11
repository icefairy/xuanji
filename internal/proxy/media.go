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
		if ferr != nil && h.health != nil {
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
		if strings.HasPrefix(model, "mimo-v2.5-tts") {
			handled, retryable, ferr = h.forwardMimoTTS(rec, r.Context(), body, up, model, i == len(candidates)-1)
		} else {
			handled, retryable, ferr = h.forwardMediaJSON(rec, r, r.Context(), body, up, model, mediaAudioSpeechPath, "audio", i == len(candidates)-1)
		}
		if ferr != nil && h.health != nil {
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
		if ferr != nil && h.health != nil {
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
			Endpoint:   endpoint,
			Status:     status,
			DurationMS: time.Since(start).Milliseconds(),
			Tokens:     0,
			APIKey:     h.recordAPIKey(r),
			ClientAddr: r.RemoteAddr, // 客户端地址 "IP:port"，用于区分调用程序
			UserAgent:  r.UserAgent(), // 客户端 UA，程序识别最强信号
		})
	}()
	reqBody := body
	if mapped := h.router.MapModel(up, model); mapped != model {
		if reqBody, err = sjson.SetBytes(body, "model", mapped); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to rewrite model field", "server_error", "")
			return true, false, nil
		}
	}

	target := strings.TrimRight(up.BaseURL, "/") + pathSuffix
	reqCtx, cancel := context.WithTimeout(ctx, upstreamTimeoutFor(h.cfg))
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target, bytes.NewReader(reqBody))
	if err != nil {
		if last {
			writeError(w, http.StatusInternalServerError, "failed to build upstream request: "+err.Error(), "server_error", "")
			return true, false, nil
		}
		return false, true, fmt.Errorf("build upstream request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+up.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		if last {
			writeError(w, http.StatusBadGateway, "upstream request failed: "+err.Error(), "server_error", "upstream_unreachable")
			return true, false, nil
		}
		return false, true, fmt.Errorf("upstream request failed: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		if last {
			h.writeUpstreamError(w, resp)
			return true, false, fmt.Errorf("upstream error: %s", resp.Status)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		return false, true, fmt.Errorf("upstream error: %s", resp.Status)
	case resp.StatusCode >= 400:
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

// forwardAudioTranscription 向上游转发 multipart 音频转录请求。
// endpoint 是记录用的端点名（"audio"）。
func (h *Handler) forwardAudioTranscription(w http.ResponseWriter, r *http.Request, rawBody []byte, up *config.Upstream, model, endpoint string, last bool) (handled, retryable bool, err error) {
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
			Endpoint:   endpoint,
			Status:     status,
			DurationMS: time.Since(start).Milliseconds(),
			Tokens:     0,
			APIKey:     h.recordAPIKey(r),
			ClientAddr: r.RemoteAddr, // 客户端地址 "IP:port"，用于区分调用程序
			UserAgent:  r.UserAgent(), // 客户端 UA，程序识别最强信号
		})
	}()
	var reqBody io.Reader
	contentType := r.Header.Get("Content-Type")

	if mapped := h.router.MapModel(up, model); mapped != model {
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
	reqCtx, cancel := context.WithTimeout(r.Context(), upstreamTimeoutFor(h.cfg))
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target, reqBody)
	if err != nil {
		if last {
			writeError(w, http.StatusInternalServerError, "failed to build upstream request: "+err.Error(), "server_error", "")
			return true, false, nil
		}
		return false, true, fmt.Errorf("build upstream request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+up.APIKey)
	req.Header.Set("Content-Type", contentType)

	resp, err := h.client.Do(req)
	if err != nil {
		if last {
			writeError(w, http.StatusBadGateway, "upstream request failed: "+err.Error(), "server_error", "upstream_unreachable")
			return true, false, nil
		}
		return false, true, fmt.Errorf("upstream request failed: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		if last {
			h.writeUpstreamError(w, resp)
			return true, false, fmt.Errorf("upstream error: %s", resp.Status)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		return false, true, fmt.Errorf("upstream error: %s", resp.Status)
	case resp.StatusCode >= 400:
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
	reqCtx, cancel := context.WithTimeout(ctx, upstreamTimeoutFor(h.cfg))
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target, bytes.NewReader(reqBody))
	if err != nil {
		if last {
			writeError(w, http.StatusInternalServerError, "failed to build upstream request: "+err.Error(), "server_error", "")
			return true, false, nil
		}
		return false, true, fmt.Errorf("build upstream request: %w", err)
	}
	req.Header.Set("api-key", up.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		if last {
			writeError(w, http.StatusBadGateway, "upstream request failed: "+err.Error(), "server_error", "upstream_unreachable")
			return true, false, nil
		}
		return false, true, fmt.Errorf("upstream request failed: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		if last {
			h.writeUpstreamError(w, resp)
			return true, false, fmt.Errorf("upstream error: %s", resp.Status)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		return false, true, fmt.Errorf("upstream error: %s", resp.Status)
	case resp.StatusCode >= 400:
		h.writeUpstreamError(w, resp)
		return true, false, nil
	default:
		respBody, rerr := io.ReadAll(resp.Body)
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
