package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/store"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// VideoCreate 处理 POST /v1/videos，转发视频生成任务到 agnes 上游
// （base_url + "/videos"），响应原样透传。
func (h *Handler) VideoCreate(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w}
	start := time.Now()

	var model, upstream string

	defer func() {
		h.log.Info("videos/create",
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
		handled, retryable, ferr := h.forwardVideoJSON(rec, r, r.Context(), body, up, model, "videos", i == len(candidates)-1)
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

// VideoQuery 处理 GET /v1/videos?video_id=xxx，查询视频任务状态。
// model 从 query 参数 ?model= 读取，缺省默认 "agnes-video-v2.0"，
// 这样走 routing_rules 的轮换池。
func (h *Handler) VideoQuery(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w}
	start := time.Now()

	var model, upstream string

	defer func() {
		h.log.Info("videos/query",
			"method", r.Method,
			"path", r.URL.Path,
			"model", model,
			"upstream", upstream,
			"status", rec.status,
			"duration", time.Since(start).String(),
		)
	}()

	videoID := r.URL.Query().Get("video_id")
	if videoID == "" {
		writeError(rec, http.StatusBadRequest, "video_id is required", "invalid_request_error", "missing_video_id")
		return
	}
	model = r.URL.Query().Get("model")
	if model == "" {
		model = "agnes-video-v2.0"
	}

	upstreams, strategy, err := h.router.Route(model)
	if err != nil {
		writeError(rec, http.StatusNotFound, fmt.Sprintf("no upstream found for model %q", model), "invalid_request_error", "model_not_found")
		return
	}

	candidates := h.selectCandidates(upstreams, strategy, model)
	for i, up := range candidates {
		handled, retryable, ferr := h.forwardVideoQuery(rec, r, videoID, up, model, "videos", i == len(candidates)-1)
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

// forwardVideoJSON 向上游转发视频生成 JSON 请求体（POST base_url + "/videos"），
// 响应原样透传。结构与 forwardMediaJSON 一致；endpoint 是记录用的端点名（"videos"）。
func (h *Handler) forwardVideoJSON(w http.ResponseWriter, r *http.Request, ctx context.Context, body []byte, up *config.Upstream, model, endpoint string, last bool) (handled, retryable bool, err error) {
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

	target := strings.TrimRight(up.BaseURL, "/") + "/videos"
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

// forwardVideoQuery 向上游转发视频任务状态查询（GET agnes 根路径 /agnesapi?video_id=xxx）。
// agnes 查询端点不在 /v1 下：base_url 形如 https://api.agnes-ai.cn/v1 时，
// 必须去掉末尾 "/v1" 再拼 "/agnesapi"。结构与 forwardMediaJSON 一致。
func (h *Handler) forwardVideoQuery(w http.ResponseWriter, r *http.Request, videoID string, up *config.Upstream, model, endpoint string, last bool) (handled, retryable bool, err error) {
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

	base := strings.TrimRight(up.BaseURL, "/")
	if strings.HasSuffix(base, "/v1") {
		base = strings.TrimSuffix(base, "/v1")
	}
	target := base + "/agnesapi?video_id=" + url.QueryEscape(videoID)
	reqCtx, cancel := context.WithTimeout(r.Context(), upstreamTimeoutFor(h.cfg))
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
	if err != nil {
		if last {
			writeError(w, http.StatusInternalServerError, "failed to build upstream request: "+err.Error(), "server_error", "")
			return true, false, nil
		}
		return false, true, fmt.Errorf("build upstream request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+up.APIKey)

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
