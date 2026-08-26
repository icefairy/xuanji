// errordetail.go：上游 4xx/5xx 错误详情日志（结构化摘要版）。
//
// 背景：messages 可能巨大（长对话历史、多图 base64），旧实现只做盲截 800 字符，
// 大请求时前 800 字全是 system 提示或图片数据，真正有用的「用户输入」与
// 「上游报错内容」被淹没。这里改为结构化摘要：
//   - request_summary：model + 最近 N 条消息（每条正文截断、图片 base64 打码占位）+
//     常用采样参数，输出为紧凑 JSON 字符串
//   - upstream_error：优先提取上游 error.message / type / code，找不到才回退原文截断
package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/tidwall/gjson"
)

const (
	detailMaxMsgContent  = 300 // 摘要中单条消息正文最大保留长度
	detailMaxMessages    = 8   // 摘要中保留的最近消息条数（更早的省略计数）
	detailMaxErrorMsg    = 4096 // 上游 error.message 最大保留长度
	detailMaxFallbackRaw = 800 // 非 JSON 请求/响应体兜底截断长度
	detailMaxSummaryJSON = 4000 // request_summary 输出上限
)

// LogUpstreamErrorDetail 把上游 4xx/5xx 错误详情打进日志（可重试与不可重试都覆盖）。
// reqBody 为发给上游的请求体（必要时已改写过的实际请求体）；respBody 为上游响应体。
// 日志格式为结构化摘要：request_summary（model + 最近消息 + 常用参数，messages 过长自动裁减、
// 图片 base64 打码占位）+ upstream_error（优先提取 error.message/type/code，找不到回退原文截断），
// 避免长对话/多图请求时盲截 800 字符刷屏。
func LogUpstreamErrorDetail(log *slog.Logger, up *config.Upstream, model, upstreamModel string, status int, reqBody, respBody []byte) {
	log.Warn("upstream 4xx/5xx detail",
		"upstream", up.Name,
		"model", model,
		"upstream_model", upstreamModel,
		"status", status,
		"request_summary", summarizeRequestBody(reqBody),
		"upstream_error", summarizeUpstreamError(respBody),
	)
}

// summarizeRequestBody 输出请求体的结构化摘要（JSON 字符串）。
func summarizeRequestBody(body []byte) string {
	if !gjson.ValidBytes(body) {
		return truncateLogStr(string(body), detailMaxFallbackRaw)
	}
	out := map[string]any{"model": gjson.GetBytes(body, "model").String()}
	if msgs := gjson.GetBytes(body, "messages"); msgs.IsArray() {
		arr := msgs.Array()
		start := 0
		if len(arr) > detailMaxMessages {
			out["messages_omitted"] = len(arr) - detailMaxMessages
			start = len(arr) - detailMaxMessages
		}
		list := make([]any, 0, len(arr)-start)
		for i := start; i < len(arr); i++ {
			m := arr[i]
			list = append(list, map[string]any{
				"role":    m.Get("role").String(),
				"content": summarizeContent(m.Get("content")),
			})
		}
		out["messages"] = list
	}
	for _, k := range []string{"max_tokens", "temperature", "top_p", "top_k",
		"reasoning_effort", "stream", "n", "presence_penalty", "frequency_penalty",
		"seed", "response_format"} {
		if v := gjson.GetBytes(body, k); v.Exists() {
			out[k] = v.Value()
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return truncateLogLine(string(body), detailMaxFallbackRaw)
	}
	return truncateLogLine(string(b), detailMaxSummaryJSON)
}

// redactImageURL 打码图片 URL：data URI 只保留 mime 前缀丢弃 base64 数据（防刷屏），
// 普通 https 链接直接截断返回。
func redactImageURL(url string) string {
	if strings.HasPrefix(url, "data:") {
		if idx := strings.Index(url, ";base64,"); idx >= 0 {
			return url[:idx] + ";<base64…>"
		}
		if idx := strings.Index(url, ","); idx >= 0 {
			return url[:idx] + ",<data…>"
		}
	}
	return truncateLogLine(url, 60)
}

// summarizeContent 摘要一条消息的 content（string 或多模态数组）。
// 图片部分用 <image <前缀>…> 占位，避免 base64 数据刷屏。
func summarizeContent(c gjson.Result) any {
	if !c.IsArray() {
		return truncateLogLine(c.String(), detailMaxMsgContent)
	}
	parts := make([]any, 0, 4)
	n := 0
	c.ForEach(func(_, p gjson.Result) bool {
		if n >= 6 {
			parts = append(parts, "...<more parts>")
			return false
		}
		n++
		switch pt := p.Get("type").String(); pt {
		case "text":
			parts = append(parts, map[string]any{
				"type": "text",
				"text": truncateLogLine(p.Get("text").String(), detailMaxMsgContent),
			})
		case "image_url":
			url := p.Get("image_url.url").String()
			parts = append(parts, map[string]any{
				"type":      "image",
				"image_url": fmt.Sprintf("<image %s…>", redactImageURL(url)),
			})
		default:
			parts = append(parts, map[string]any{"type": pt})
		}
		return true
	})
	return parts
}

// summarizeUpstreamError 提取上游错误摘要：优先 error.message（带 type/code），
// 找不到 error 字段时回退原文截断。
func summarizeUpstreamError(body []byte) string {
	if gjson.ValidBytes(body) {
		if e := gjson.GetBytes(body, "error"); e.Exists() {
			msg := e.Get("message").String()
			tp := e.Get("type").String()
			code := e.Get("code").String()
	// 压缩换行/制表，便于在日志单行查看
			msg = strings.Join(strings.Fields(msg), " ")
			var sb strings.Builder
			sb.WriteString("message=")
			sb.WriteString(truncateLogLine(msg, detailMaxErrorMsg))
			if tp != "" {
				sb.WriteString(" type=")
				sb.WriteString(tp)
			}
			if code != "" {
				sb.WriteString(" code=")
				sb.WriteString(code)
			}
			return sb.String()
		}
	}
	return truncateLogLine(string(body), detailMaxFallbackRaw)
}

// truncateLogLine 截断字符串为单行便于日志查看（换行压缩为空格）。
func truncateLogLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max] + "…(truncated)"
	}
	return s
}
// consumeUpstreamError 读取上游 4xx/5xx 响应体：先打结构化错误详情日志
// （请求摘要 + 响应摘要，messages 过长自动精简、图片 base64 打码），
// 再把读取到的内容放回 resp.Body，供后续 writeUpstreamError 等透传错误响应用
// （避免 double-read 与丢 body）。适用于 media/video/rerank/embeddings 等
// 未单独解析响应体的转发端点；chat 主路径已自行读取 body 后直接调用 LogUpstreamErrorDetail。
func (h *Handler) consumeUpstreamError(resp *http.Response, up *config.Upstream, model, upstreamModel string, reqBody []byte) {
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body = io.NopCloser(bytes.NewReader(respBody))
	LogUpstreamErrorDetail(h.log, up, model, upstreamModel, resp.StatusCode, reqBody, respBody)
}
