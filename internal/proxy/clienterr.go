package proxy

import (
	"errors"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// errClientRequest 是哨兵错误：本次转发失败由客户端请求本身引起（模型名不存在、
// 输入超长等），上游只是合法拒绝了该请求，上游本身健康。调用方（转发循环）
// 识别后不把该失败计入上游健康（MarkFailure）——否则合法拒绝也会把健康上游
// 打成 degraded/dead 并逐出转发候选。仍保留可重试语义（换上游可能成功）。
var errClientRequest = errors.New("client request error (upstream rejected this request)")

// clientRequestError 包装一次「客户端请求问题」导致的转发失败，使其可通过
// errors.Is(err, errClientRequest) 与真实上游故障区分。错误文本保持原样，
// 不影响既有 error_detail 落库与日志。
type clientRequestError struct{ err error }

func (e *clientRequestError) Error() string   { return e.err.Error() }
func (e *clientRequestError) Unwrap() error   { return e.err }
func (e *clientRequestError) Is(t error) bool { return t == errClientRequest }

// clientErrReasons 是「客户端请求本身的问题」类上游错误特征（小写子串匹配）。
//
// 判定依据：错误由本次请求的内容/参数引起，与上游是否健康无关——同一上游换一个
// 合法请求即可成功。此类错误不应计入上游健康失败（会把健康上游打成 degraded/dead）
// 或 fastfail 黑名单（会把健康上游踢出转发候选）。
//
// 例（2026-09-12 日志实测）：
//   - wechat 对未提供的模型名回 400 `invalid model: model name not found`
//   - 各家输入超长回 400 `The input (N tokens) is longer than the model's context length`
//     / `exceeds the model's maximum context length of M tokens` / `input length too long`
//
// 刻意不含鉴权类（key 无效）与配额类（余额不足/额度耗尽）：那些是上游账号/资源
// 状态问题，不会因换请求而自愈，由 arrear 机制与 fastfail 保留黑名单处理。
var clientErrReasons = []string{
	// 模型名不存在/不支持（客户端请求了该上游不提供的模型）
	"model name not found",
	"invalid model",
	"model not found",
	"is not supported",
	"does not exist",
	"no such model",
	// 输入超长（客户端上下文超模型窗口；网关透传 400 让客户端自省，非上游故障）
	"longer than the model's context length",
	"maximum context length",
	"input length too long",
	"context_length_exceeded",
	// 请求参数校验失败（客户端传了空数组/缺必填字段等；上游正确校验后拒绝）
	// 例（2026-09-13 实测，硅基流动 rerank/embeddings）：
	//   `List should have at least 1 item after validation, not 0`（documents/input 为空数组）
	//   `Field required`（缺 query/documents/input）
	//   `The parameter is invalid. Please check again.`（参数非法）
	"list should have at least 1 item after validation",
	"field required",
	"the parameter is invalid",
}

// isClientRequestError 判断一次上游 4xx 错误响应是否属于「客户端请求本身的问题」。
//
// 仅在 4xx（且非 401/402/403 鉴权类）时考虑：客户端请求的内容/参数被上游合法拒绝，
// 上游本身健康。返回 true 时调用方应跳过 fastfail 标记与健康失败计数。
func isClientRequestError(status int, respBody []byte) bool {
	if status < 400 || status >= 500 {
		return false
	}
	// 鉴权/配额类不算：key 无效、余额不足是上游账号状态，不会因换请求而自愈。
	switch status {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden:
		return false
	}
	msg := strings.ToLower(upstreamErrorText(respBody))
	for _, kw := range clientErrReasons {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

// upstreamErrorText 提取上游错误响应体中可判读的文本用于特征匹配：
// 优先 error.message / message / msg / detail（各家格式不一），取不到时回退响应体原文。
func upstreamErrorText(respBody []byte) string {
	for _, path := range []string{"error.message", "message", "msg", "detail"} {
		if s := gjson.GetBytes(respBody, path).String(); s != "" {
			return s
		}
	}
	return string(respBody)
}
