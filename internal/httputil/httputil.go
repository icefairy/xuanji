// Package httputil 提供网关各协议包共用的 HTTP 辅助函数。
package httputil

import (
	"context"
	"errors"
	"io"
)

// MaxBodyBytes 是读取上游响应体的上限（64MB）。异常/失陷上游返回超大 body 时
// 截断，避免 io.ReadAll 无界读取打爆内存（SSE 流式路径逐行透传，不受此限）。
const MaxBodyBytes = 64 << 20

// ErrBodyTooLarge 表示响应体超过 MaxBodyBytes 被截断。
var ErrBodyTooLarge = errors.New("upstream response body too large")

// ReadBody 读取 r 并施加上限。超过上限时返回已读到的前缀 + ErrBodyTooLarge，
// 调用方应视为上游失败，而非把残缺 body 当成功透传。
func ReadBody(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxBodyBytes+1))
	if err != nil {
		return data, err
	}
	if int64(len(data)) > MaxBodyBytes {
		return data[:MaxBodyBytes], ErrBodyTooLarge
	}
	return data, nil
}

// UpstreamErrorMessage 生成可安全返回给客户端的上游错误摘要。
// 不直接回传 err.Error()：*url.Error 会带完整上游 URL/IP（如
// `Post "http://10.0.0.5:8000/...": dial tcp 10.0.0.5:8000: connection refused`），
// 泄露内网上游拓扑。此处仅给出类别，细节由调用方记入服务端日志。
func UpstreamErrorMessage(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "upstream request timed out"
	case errors.Is(err, context.Canceled):
		return "request canceled"
	default:
		return "upstream request failed"
	}
}
