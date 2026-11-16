package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/icefairy/xuanji/internal/config"
)

// 云知声（Unisound）U2-ASR / U2-TTS 异步适配。
//
// ## 为什么需要单独适配
//
// 云知声的语音接口和 OpenAI 标准协议**完全不是一回事**：
//
//	能力     OpenAI 标准                      云知声 U2
//	TTS      POST /v1/audio/speech             POST /v1/audio/speech/tasks（异步任务）
//	         （同步直接返回音频二进制）        → GET  /v1/audio/speech/tasks?task_id=
//	                                          → GET  /v1/files/retrieve_content?file_id=
//	ASR      POST /v1/audio/transcriptions     POST /v1/files/upload（先传文件换 file_id）
//	         （multipart 一次请求）            → POST /v1/audio/asr/tasks
//	                                          → GET  /v1/audio/asr/tasks/{task_id}
//
// 直接把请求转发到 /v1/audio/speech 必然 404——上游根本没这个端点。
// 这里做协议转换：对外保持 OpenAI 契约，对内走云知声三步异步流程。
//
// ## 轮询策略
//
// 实测（4.3 秒音频）：
//   TTS 创建 → 查询，首次即 Success
//   ASR 创建 → Waiting(0s) → Processing(3-6s) → Waiting(9-15s) → Success(18s)
// ASR 会短暂回退到 Waiting（后端排队重排），所以**不能见到 Waiting 就判失败**，
// 必须一直轮询到 Success/Failed 或超时。

const (
	// 轮询间隔：首次 300ms 快速拿到已完成的短任务，之后退避到 1s
	pollInitialInterval = 300 * time.Millisecond
	pollMaxInterval     = 1500 * time.Millisecond

	// 总轮询上限。ASR 实测 18s，长音频会更久；给足 5 分钟。
	pollTimeout = 5 * time.Minute
)

// isUnisoundAsyncModel 判断该模型是否要走云知声异步适配。
//
// 只认 u2-asr / u2-tts 前缀，避免影响其他上游的同名模型。
// u2-tts-clone / u2-tts-design 也走同一套任务流程，一并覆盖。
func isUnisoundAsyncModel(model string) bool {
	m := strings.ToLower(model)
	return m == "u2-asr" || strings.HasPrefix(m, "u2-tts")
}

// isLocalTTSUpstream 判断上游是否为本地兜底 TTS 服务（piper）。
//
// 必须靠 kind 显式标记，不能靠 base_url 猜：httptest 起的假上游和真实内网
// 上游都可能是 127.0.0.1 / 192.168.*，按地址猜会把 mimo 等云厂商分支误判。
// 本地兜底上游在数据库里把 kind 设为 "tts" 且名字以 piper 开头。
func isLocalTTSUpstream(up *config.Upstream) bool {
	return strings.HasPrefix(strings.ToLower(up.Name), "piper") ||
		strings.EqualFold(up.Kind, "tts-local")
}

// isUnisoundUpstream 判断上游是否为云知声（按 base_url 识别，不靠名字）。
func isUnisoundUpstream(up *config.Upstream) bool {
	return strings.Contains(strings.ToLower(up.BaseURL), "unisound")
}

// ---------------------------------------------------------------------------
// TTS: OpenAI /v1/audio/speech → 云知声异步任务
// ---------------------------------------------------------------------------

// forwardUnisoundTTS 把 OpenAI 格式的 TTS 请求转成云知声异步任务并取回音频。
//
// 对外契约保持 OpenAI：
//
//	请求  {model, input, voice, response_format, speed}
//	响应  音频二进制（Content-Type: audio/mpeg）
func (h *Handler) forwardUnisoundTTS(w http.ResponseWriter, ctx context.Context, body []byte, up *config.Upstream, model string, last bool) (handled, retryable bool, err error) {
	input := gjson.GetBytes(body, "input").String()
	if input == "" {
		// 兼容部分客户端用 text 字段
		input = gjson.GetBytes(body, "text").String()
	}
	if input == "" {
		if last {
			writeError(w, http.StatusBadRequest, "input is required", "invalid_request_error", "missing_input")
			return true, false, nil
		}
		return false, true, fmt.Errorf("missing input")
	}

	format := gjson.GetBytes(body, "response_format").String()
	if format == "" {
		format = "mp3"
	}
	// 云知声只支持 mp3 / pcm
	if format != "mp3" && format != "pcm" {
		format = "mp3"
	}

	// 音色映射：OpenAI 标准音色 → 云知声音色
	voice := mapUnisoundVoice(gjson.GetBytes(body, "voice").String())

	speed := gjson.GetBytes(body, "speed").Float()
	if speed <= 0 {
		speed = 1.0
	}
	// 云知声 speed 是 0-100 的整数，默认 50；OpenAI 是 0.25-4.0 倍率
	speedInt := int(speed * 50)
	if speedInt < 0 {
		speedInt = 0
	}
	if speedInt > 100 {
		speedInt = 100
	}

	payload := map[string]any{
		"model": model,
		"text":  input,
		"voice_setting": map[string]any{
			"voice_id": voice,
			"speed":    speedInt,
			"volume":   50,
			"pitch":    50,
			"language": "zh",
		},
		"audio_setting": map[string]any{
			"audio_sample_rate": 24000,
			"format":            format,
			"channel":           1,
		},
	}
	reqBody, merr := json.Marshal(payload)
	if merr != nil {
		if last {
			writeError(w, http.StatusInternalServerError, "build unisound tts body: "+merr.Error(), "server_error", "")
			return true, false, nil
		}
		return false, true, fmt.Errorf("build body: %w", merr)
	}

	// 第 1 步：创建任务
	respBody, status, cerr := h.unisoundJSON(ctx, up, http.MethodPost, "/audio/speech/tasks", reqBody, "application/json")
	if cerr != nil {
		if last {
			writeError(w, http.StatusBadGateway, upstreamErrorMessage(cerr), "server_error", "upstream_unreachable")
			return true, false, nil
		}
		return false, true, cerr
	}
	if status >= 400 {
		h.logUnisoundFailure(up, model, status, respBody, body)
		if last {
			writeError(w, status, unisoundErrMessage(respBody, "tts task create failed"), "upstream_error", "")
			return true, false, nil
		}
		return false, status >= 500, fmt.Errorf("upstream status %d", status)
	}

	taskID := gjson.GetBytes(respBody, "task_id").String()
	if taskID == "" {
		if last {
			writeError(w, http.StatusBadGateway, "unisound returned no task_id", "server_error", "")
			return true, false, nil
		}
		return false, true, fmt.Errorf("no task_id")
	}

	// 第 2 步：轮询任务状态，拿到 file_id
	fileID, perr := h.pollUnisoundTask(ctx, up, "/audio/speech/tasks", taskID)
	if perr != nil {
		if last {
			writeError(w, http.StatusBadGateway, "poll tts task: "+perr.Error(), "server_error", "upstream_timeout")
			return true, false, nil
		}
		return false, true, perr
	}

	// 第 3 步：下载音频
	audio, derr := h.downloadUnisoundFile(ctx, up, fileID)
	if derr != nil {
		if last {
			writeError(w, http.StatusBadGateway, "download tts audio: "+derr.Error(), "server_error", "")
			return true, false, nil
		}
		return false, true, derr
	}

	w.Header().Set("Content-Type", "audio/"+format)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(audio)))
	w.WriteHeader(http.StatusOK)
	if _, werr := w.Write(audio); werr != nil {
		h.log.Debug("write unisound tts audio", "error", werr)
	}
	return true, false, nil
}

// ---------------------------------------------------------------------------
// ASR: OpenAI /v1/audio/transcriptions → 云知声异步任务
// ---------------------------------------------------------------------------

// forwardUnisoundASR 把 OpenAI 格式的转写请求转成云知声异步任务。
//
// 对外契约保持 OpenAI：
//
//	请求  multipart: file + model [+ language, response_format]
//	响应  {text} 或 {text, segments:[{start,end,text}]}
//
// 云知声要求先上传文件换 file_id，所以这里要多一步：
// 把 multipart 里的音频重新打包上传到 /v1/files/upload。
func (h *Handler) forwardUnisoundASR(w http.ResponseWriter, r *http.Request, up *config.Upstream, model string, last bool) (handled, retryable bool, err error) {
	ctx := r.Context()

	// 从 multipart 里取音频
	file, header, ferr := r.FormFile("file")
	if ferr != nil {
		if last {
			writeError(w, http.StatusBadRequest, "file field is required: "+ferr.Error(), "invalid_request_error", "missing_file")
			return true, false, nil
		}
		return false, true, ferr
	}
	defer file.Close()

	audioBytes, rerr := io.ReadAll(file)
	if rerr != nil {
		if last {
			writeError(w, http.StatusBadRequest, "read uploaded file: "+rerr.Error(), "invalid_request_error", "")
			return true, false, nil
		}
		return false, true, rerr
	}
	if len(audioBytes) == 0 {
		if last {
			writeError(w, http.StatusBadRequest, "uploaded file is empty", "invalid_request_error", "")
			return true, false, nil
		}
		return false, true, fmt.Errorf("empty file")
	}

	filename := "audio.mp3"
	if header != nil && header.Filename != "" {
		filename = header.Filename
	}
	format := audioFormatFromName(filename)

	// 第 1 步：上传音频换 file_id
	fileID, uerr := h.uploadUnisoundFile(ctx, up, filename, audioBytes, "a2t_async_input")
	if uerr != nil {
		if last {
			writeError(w, http.StatusBadGateway, "upload to unisound: "+uerr.Error(), "server_error", "")
			return true, false, nil
		}
		return false, true, uerr
	}

	// 第 2 步：创建转写任务
	payload := map[string]any{
		"file_id":     fileID,
		"model":       model,
		"format":      format,
		"enable_itn":  true,
		"channel":     1,
		"word_info":   false,
		"enable_auto_lang": true, // 自动识别语种，省得客户端指定
	}
	// 客户端显式指定语言时优先
	if lang := r.FormValue("language"); lang != "" {
		payload["language"] = mapOpenAILanguage(lang)
		delete(payload, "enable_auto_lang")
	}
	reqBody, merr := json.Marshal(payload)
	if merr != nil {
		if last {
			writeError(w, http.StatusInternalServerError, "build asr body: "+merr.Error(), "server_error", "")
			return true, false, nil
		}
		return false, true, merr
	}

	respBody, status, cerr := h.unisoundJSON(ctx, up, http.MethodPost, "/audio/asr/tasks", reqBody, "application/json")
	if cerr != nil {
		if last {
			writeError(w, http.StatusBadGateway, upstreamErrorMessage(cerr), "server_error", "upstream_unreachable")
			return true, false, nil
		}
		return false, true, cerr
	}
	if status >= 400 {
		h.logUnisoundFailure(up, model, status, respBody, reqBody)
		if last {
			writeError(w, status, unisoundErrMessage(respBody, "asr task create failed"), "upstream_error", "")
			return true, false, nil
		}
		return false, status >= 500, fmt.Errorf("upstream status %d", status)
	}

	taskID := gjson.GetBytes(respBody, "task_id").String()
	if taskID == "" {
		if last {
			writeError(w, http.StatusBadGateway, "unisound returned no task_id", "server_error", "")
			return true, false, nil
		}
		return false, true, fmt.Errorf("no task_id")
	}

	// 第 3 步：轮询直到 Success，拿到 results
	resultBody, perr := h.pollUnisoundASRResult(ctx, up, taskID)
	if perr != nil {
		if last {
			writeError(w, http.StatusBadGateway, "poll asr task: "+perr.Error(), "server_error", "upstream_timeout")
			return true, false, nil
		}
		return false, true, perr
	}

	// 第 4 步：转成 OpenAI 格式
	writeOpenAITranscription(w, resultBody, gjson.GetBytes(reqBody, "model").String())
	return true, false, nil
}

// ---------------------------------------------------------------------------
// 云知声 HTTP 辅助
// ---------------------------------------------------------------------------

// unisoundJSON 发起 JSON 请求，返回 body 和状态码。
func (h *Handler) unisoundJSON(ctx context.Context, up *config.Upstream, method, path string, body []byte, contentType string) ([]byte, int, error) {
	target := strings.TrimRight(up.BaseURL, "/") + path

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	reqCtx, cancel := context.WithTimeout(ctx, upstreamTimeoutForUp(up, h.cfg))
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, method, target, reader)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+up.APIKey)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	respBody, rerr := readUpstreamBody(resp.Body)
	if rerr != nil {
		return nil, resp.StatusCode, fmt.Errorf("read body: %w", rerr)
	}
	return respBody, resp.StatusCode, nil
}

// uploadUnisoundFile 上传文件到云知声，返回 file_id。
func (h *Handler) uploadUnisoundFile(ctx context.Context, up *config.Upstream, filename string, data []byte, purpose string) (int64, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	if err := mw.WriteField("purpose", purpose); err != nil {
		return 0, err
	}
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return 0, err
	}
	if _, err := part.Write(data); err != nil {
		return 0, err
	}
	if err := mw.Close(); err != nil {
		return 0, err
	}

	target := strings.TrimRight(up.BaseURL, "/") + "/files/upload"
	reqCtx, cancel := context.WithTimeout(ctx, upstreamTimeoutForUp(up, h.cfg))
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target, &buf)
	if err != nil {
		return 0, fmt.Errorf("build upload request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+up.APIKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := h.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	respBody, rerr := readUpstreamBody(resp.Body)
	if rerr != nil {
		return 0, fmt.Errorf("read upload response: %w", rerr)
	}
	if resp.StatusCode >= 400 {
		return 0, fmt.Errorf("upload failed status=%d body=%s", resp.StatusCode, truncate(string(respBody), 300))
	}

	// 响应结构：{"file":{"file_id":123,...},"base_resp":{...}}
	fid := gjson.GetBytes(respBody, "file.file_id").Int()
	if fid == 0 {
		// 兜底：顶层 file_id
		fid = gjson.GetBytes(respBody, "file_id").Int()
	}
	if fid == 0 {
		return 0, fmt.Errorf("no file_id in upload response: %s", truncate(string(respBody), 300))
	}
	return fid, nil
}

// pollUnisoundTask 轮询 TTS 任务直到完成，返回 file_id。
func (h *Handler) pollUnisoundTask(ctx context.Context, up *config.Upstream, path, taskID string) (int64, error) {
	deadline := time.Now().Add(pollTimeout)
	interval := pollInitialInterval

	for {
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("task %s timeout after %s", taskID, pollTimeout)
		}

		query := fmt.Sprintf("%s?task_id=%s", path, taskID)
		body, status, err := h.unisoundJSON(ctx, up, http.MethodGet, query, nil, "")
		if err != nil {
			return 0, err
		}
		if status >= 400 {
			return 0, fmt.Errorf("query status=%d body=%s", status, truncate(string(body), 200))
		}

		switch gjson.GetBytes(body, "status").String() {
		case "Success":
			fid := gjson.GetBytes(body, "file_id").Int()
			if fid == 0 {
				return 0, fmt.Errorf("task success but no file_id: %s", truncate(string(body), 200))
			}
			return fid, nil
		case "Failed":
			return 0, fmt.Errorf("task failed: %s", truncate(string(body), 300))
		}
		// Waiting / Processing：继续等

		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(interval):
		}
		if interval < pollMaxInterval {
			interval = interval * 3 / 2
			if interval > pollMaxInterval {
				interval = pollMaxInterval
			}
		}
	}
}

// pollUnisoundASRResult 轮询 ASR 任务直到 Success，返回完整结果体。
func (h *Handler) pollUnisoundASRResult(ctx context.Context, up *config.Upstream, taskID string) ([]byte, error) {
	deadline := time.Now().Add(pollTimeout)
	interval := pollInitialInterval

	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("asr task %s timeout after %s", taskID, pollTimeout)
		}

		path := "/audio/asr/tasks/" + taskID
		body, status, err := h.unisoundJSON(ctx, up, http.MethodGet, path, nil, "")
		if err != nil {
			return nil, err
		}
		if status >= 400 {
			return nil, fmt.Errorf("query status=%d body=%s", status, truncate(string(body), 200))
		}

		switch gjson.GetBytes(body, "status").String() {
		case "Success":
			return body, nil
		case "Failed":
			return nil, fmt.Errorf("asr task failed: %s", truncate(string(body), 300))
		}
		// Waiting / Processing：ASR 会在两者之间来回跳，必须继续等

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
		if interval < pollMaxInterval {
			interval = interval * 3 / 2
			if interval > pollMaxInterval {
				interval = pollMaxInterval
			}
		}
	}
}

// downloadUnisoundFile 下载云知声生成的文件内容。
func (h *Handler) downloadUnisoundFile(ctx context.Context, up *config.Upstream, fileID int64) ([]byte, error) {
	path := fmt.Sprintf("/files/retrieve_content?file_id=%d", fileID)
	body, status, err := h.unisoundJSON(ctx, up, http.MethodGet, path, nil, "")
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("download status=%d body=%s", status, truncate(string(body), 200))
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("downloaded file is empty")
	}
	return body, nil
}

// logUnisoundFailure 记录上游失败细节，便于排查。
func (h *Handler) logUnisoundFailure(up *config.Upstream, model string, status int, respBody, reqBody []byte) {
	h.log.Warn("unisound upstream error",
		"upstream", up.Name,
		"model", model,
		"status", status,
		"body", truncate(string(respBody), 400),
	)
}

// ---------------------------------------------------------------------------
// 响应转换
// ---------------------------------------------------------------------------

// writeOpenAITranscription 把云知声 ASR 结果转成 OpenAI /v1/audio/transcriptions 格式。
//
// 云知声结果结构：
//
//	{"status":"Success","duration":4296,"results":[
//	   {"text":"你好，","start":224,"end":464,"speaker":"0"},
//	   {"text":"这是测试语音。","start":864,"end":3744,"speaker":"0"}]}
//
// OpenAI 格式：
//
//	{"text":"完整文本"}  或  {"text":"...","segments":[{"start":0.224,"end":0.464,"text":"你好，"}]}
func writeOpenAITranscription(w http.ResponseWriter, body []byte, model string) {
	var sb strings.Builder
	type segment struct {
		ID      int     `json:"id"`
		Start   float64 `json:"start"`
		End     float64 `json:"end"`
		Text    string  `json:"text"`
		Speaker string  `json:"speaker,omitempty"`
	}
	var segments []segment

	results := gjson.GetBytes(body, "results")
	if results.IsArray() {
		idx := 0
		results.ForEach(func(_, item gjson.Result) bool {
			text := item.Get("text").String()
			if text != "" {
				sb.WriteString(text)
			}
			segments = append(segments, segment{
				ID:      idx,
				Start:   item.Get("start").Float() / 1000.0, // 毫秒 → 秒
				End:     item.Get("end").Float() / 1000.0,
				Text:    text,
				Speaker: item.Get("speaker").String(),
			})
			idx++
			return true
		})
	}

	resp := map[string]any{
		"text":  sb.String(),
		"model": model,
	}
	if len(segments) > 0 {
		resp["segments"] = segments
	}
	// 音频时长（秒），OpenAI 有 duration 字段
	if dur := gjson.GetBytes(body, "duration").Float(); dur > 0 {
		resp["duration"] = dur / 1000.0
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// ---------------------------------------------------------------------------
// 映射与工具
// ---------------------------------------------------------------------------

// mapUnisoundVoice 把 OpenAI 标准音色映射到云知声音色。
// 客户端传云知声原生 voice_id 时原样透传。
func mapUnisoundVoice(voice string) string {
	switch voice {
	case "", "alloy", "nova", "shimmer":
		return "cn_female_shasha" // 女声，通用场景
	case "echo", "onyx", "fable":
		return "cn_male_chenyu" // 男声
	default:
		return voice // 已是云知声 voice_id
	}
}

// mapOpenAILanguage 把 OpenAI 语言码转成云知声语言码。
func mapOpenAILanguage(lang string) string {
	switch strings.ToLower(lang) {
	case "zh", "zh-cn", "chinese":
		return "zh-CN"
	case "en", "en-us", "english":
		return "en-US"
	case "ja", "ja-jp", "japanese":
		return "ja-JP"
	case "ko", "ko-kr", "korean":
		return "ko-KR"
	default:
		return "zh-CN"
	}
}

// audioFormatFromName 从文件名推断音频格式。
func audioFormatFromName(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".mp3"):
		return "mp3"
	case strings.HasSuffix(lower, ".wav"):
		return "wav"
	case strings.HasSuffix(lower, ".m4a"):
		return "m4a"
	case strings.HasSuffix(lower, ".ogg"):
		return "ogg"
	case strings.HasSuffix(lower, ".opus"):
		return "opus"
	case strings.HasSuffix(lower, ".amr"):
		return "amr"
	default:
		return "mp3"
	}
}

// unisoundErrMessage 从云知声错误体里取可读信息。
func unisoundErrMessage(body []byte, fallback string) string {
	if msg := gjson.GetBytes(body, "base_resp.status_msg").String(); msg != "" {
		return "unisound: " + msg
	}
	if msg := gjson.GetBytes(body, "error.message").String(); msg != "" {
		return "unisound: " + msg
	}
	if msg := gjson.GetBytes(body, "message").String(); msg != "" {
		return "unisound: " + msg
	}
	return fallback
}

// truncate 截断长字符串用于日志/错误消息。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
