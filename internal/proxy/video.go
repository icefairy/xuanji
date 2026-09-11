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
// （base_url + "/videos"）。请求体全参数透传（prompt/mode/seconds/n/size/
// aspect_ratio/seed/first_frame/last_frame/images/audios/videos 等，兼容 Agnes
// 扩展字段与 OpenAI Videos 协议公共子集），仅按上游 model_mapping 改写模型名。
// 创建成功后从响应提取 video_id 落库 video_jobs 表（video_id → 上游+上游真实
// 模型名），供后续查询自动补 agnes 必需的 model_name 参数并定向承接上游。
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

// saveVideoJob 视频任务创建成功后落库归属记录（recorder/store 为 nil 时静默跳过）。
func (h *Handler) saveVideoJob(videoID, upstream, upstreamModel, clientModel string) {
	if h.recorder == nil || videoID == "" {
		return
	}
	if s := h.recorder.Store(); s != nil {
		if err := s.UpsertVideoJob(videoID, upstream, upstreamModel, clientModel); err != nil {
			h.log.Warn("save video job failed", "video_id", videoID, "error", err)
		}
	}
}

// resolveVideoUpstream 查询任务的定向解析：优先用本地 video_jobs 记录
// （创建时落库的归属：直接返回承接上游 + 上游真实模型名）；无记录时回退
// 按客户端 model 路由选候选列表（model_name 尽力 MapModel 映射；多映射竖线
// 随机值可能与创建时不一致，仅影响非 text 模式查询，提示客户端重发创建请求
// 或传 model 参数可缓解）。
// 返回候选列表 + 查询 URL 的 model_name 参数值（空=不传）。
func (h *Handler) resolveVideoUpstream(videoID, clientModel string) ([]*config.Upstream, string, string, bool) {
	if h.recorder != nil {
		if s := h.recorder.Store(); s != nil {
			if job, jerr := s.GetVideoJob(videoID); jerr == nil {
				for i := range h.cfg.Upstreams {
					if h.cfg.Upstreams[i].Name == job.Upstream {
						up := &h.cfg.Upstreams[i]
						mn := job.UpstreamModel
						if mn == "" {
							mn = h.router.MapModel(up, job.ClientModel)
						}
						return []*config.Upstream{up}, mn, job.ClientModel, true
					}
				}
				// 归属上游已被删除：回退路由（打日志便于发现配置漂移）
				h.log.Warn("video job upstream missing, fallback to routing",
					"video_id", videoID, "job_upstream", job.Upstream)
			}
		}
	}
	ups, strategy, err := h.router.Route(clientModel)
	if err != nil {
		return nil, "", clientModel, false
	}
	var modelName string
	if len(ups) > 0 {
		modelName = h.router.MapModel(ups[0], clientModel)
	}
	// 回退路径与创建请求同一套候选过滤（禁用/欠费/冷却/健康），保持行为一致
	return h.selectCandidates(ups, strategy, clientModel), modelName, clientModel, false
}

// VideoQuery 处理 GET /v1/videos?video_id=xxx[&model=xxx]，查询视频任务状态。
// 定向规则：本地有该 video_id 的归属记录时直连承接上游（无需客户端传 model）——
// agnes 要求 keyframe/reference 模式查询必须带 model_name（映射后的上游真实名），
// 由创建时的落库记录自动补齐；无记录时按 ?model= 路由（必传，缺省硬编码已废弃）。
func (h *Handler) VideoQuery(w http.ResponseWriter, r *http.Request) {
	h.videoQueryByID(w, r, r.URL.Query().Get("video_id"))
}

// VideoGetByID 处理 GET /v1/videos/{id}（OpenAI Videos 协议标准路径形式），
// 与 VideoQuery 等价：query 参数式的别名，内部同一套定向/透传逻辑。
func (h *Handler) VideoGetByID(w http.ResponseWriter, r *http.Request) {
	h.videoQueryByID(w, r, r.PathValue("id"))
}

// videoQueryByID 是视频任务状态查询的统一入口（videoID 来自 query 或 path）。
func (h *Handler) videoQueryByID(w http.ResponseWriter, r *http.Request, videoID string) {
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

	if videoID == "" {
		writeError(rec, http.StatusBadRequest, "video_id is required", "invalid_request_error", "missing_video_id")
		return
	}
	clientModel := r.URL.Query().Get("model")
	cands, modelName, routedModel, fromJob := h.resolveVideoUpstream(videoID, clientModel)
	if len(cands) == 0 {
		msg := fmt.Sprintf("no upstream found for video_id %q", videoID)
		if !fromJob && clientModel == "" {
			// 无本地记录且未指定 model：老行为缺省 "agnes-video-v2.0" 会静默查错渠道，
			// 改为显式报错引导（新创建的任务已自动落库归属，无需再传）。
			msg = "model is required for querying videos without a local creation record; pass ?model=<name> (newly created tasks are tracked automatically)"
		}
		writeError(rec, http.StatusBadRequest, msg, "invalid_request_error", "model_not_found")
		return
	}
	model = routedModel

	for i, up := range cands {
		handled, retryable, ferr := h.forwardVideoQuery(rec, r, videoID, modelName, up, model, "videos", i == len(cands)-1)
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
			ClientAddr: r.RemoteAddr,  // 客户端地址 "IP:port"，用于区分调用程序
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
		// 读出成功响应体提取 video_id 落库归属后写回客户端：
		// 任务 JSON 很小（KB 级），先读全量无压力。
		respBody, rerr := readUpstreamBody(resp.Body)
		if rerr != nil {
			writeError(w, http.StatusBadGateway, "failed to read upstream response: "+rerr.Error(), "server_error", "upstream_unreachable")
			return true, false, nil
		}
		vid := gjson.GetBytes(respBody, "video_id").String()
		if vid == "" {
			// 兼容仅含 id/task_id 的实现：取第一个非空任务 ID 字段
			vid = gjson.GetBytes(respBody, "task_id").String()
		}
		if vid != "" {
			h.saveVideoJob(vid, up.Name, gjson.GetBytes(reqBody, "model").String(), model)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		if _, cerr := w.Write(respBody); cerr != nil {
			h.log.Debug("copy upstream body", "error", cerr)
		}
		return true, false, nil
	}
}

// forwardVideoQuery 向上游转发视频任务状态查询（GET agnes 根路径 /agnesapi）。
// agnes 文档：推荐 video_id+model_name 组合，keyframe/reference 模式必须带
// model_name（值为映射后的上游真实模型名）。modelName 为空时不拼该参数
// （纯 video_id 查询仅 text 模式有效，属旧行为兼容）。
// 必须去掉末尾 "/v1" 再拼 "/agnesapi"。结构与 forwardMediaJSON 一致。
func (h *Handler) forwardVideoQuery(w http.ResponseWriter, r *http.Request, videoID, modelName string, up *config.Upstream, model, endpoint string, last bool) (handled, retryable bool, err error) {
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
			ClientAddr: r.RemoteAddr,  // 客户端地址 "IP:port"，用于区分调用程序
			UserAgent:  r.UserAgent(), // 客户端 UA，程序识别最强信号
		})
	}()

	base := strings.TrimRight(up.BaseURL, "/")
	if strings.HasSuffix(base, "/v1") {
		base = strings.TrimSuffix(base, "/v1")
	}
	target := base + "/agnesapi?video_id=" + url.QueryEscape(videoID)
	if modelName != "" {
		target += "&model_name=" + url.QueryEscape(modelName)
	}
	reqCtx, cancel := context.WithTimeout(r.Context(), upstreamTimeoutForUp(up, h.cfg))
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
	if err != nil {
		if last {
			writeError(w, http.StatusInternalServerError, "failed to build upstream request", "server_error", "")
			return true, false, nil
		}
		return false, true, fmt.Errorf("build upstream request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+up.APIKey)

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
			h.consumeUpstreamError(resp, up, model, model, nil)
			h.writeUpstreamError(w, resp)
			return true, false, fmt.Errorf("upstream error: %s", resp.Status)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		return false, true, fmt.Errorf("upstream error: %s", resp.Status)
	case resp.StatusCode >= 400:
		h.consumeUpstreamError(resp, up, model, model, nil)
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
