package proxy

import (
	"fmt"
	"net/http"
	"testing"
)

// upstreamStatusFromErr 从错误串里反推上游 HTTP 状态码，用于失败落库
// （recordRetryableFailure）与确定性错误判定（isDeterministicClientStatus）。
//
// 2026-10-30 实测：agnes 系当日 429 限流 68 次，request_log 里全部记成 status=0，
// 管理页状态分布看不到 429——因为 429 分支返回的是 "upstream rate limited: 429 …"，
// 而本函数只认 "upstream error: " 前缀。
func TestUpstreamStatusFromErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{
			// 通用 4xx/5xx 走 retryableUpstreamError("upstream error", …)
			name: "通用路径 500",
			err:  fmt.Errorf("upstream error: %s: message=Internal Server Error", "500 Internal Server Error"),
			want: http.StatusInternalServerError,
		},
		{
			// 通用路径的 429 也走这里（403/401 等）
			name: "通用路径 403",
			err:  fmt.Errorf("upstream error: %s", "403 Forbidden"),
			want: http.StatusForbidden,
		},
		{
			// 2026-10-30 实测：proxy.go:1541 返回 "upstream rate limited: 429 Too Many Requests"。
			// 修复前这里返回 0，导致 429 失败在 request_log 里全部记成 status=0。
			name: "429 限流专用分支（proxy.go）",
			err:  fmt.Errorf("upstream rate limited: %s", "429 Too Many Requests"),
			want: http.StatusTooManyRequests,
		},
		{
			// completions.go:250 的同类分支
			name: "429 限流专用分支（completions.go）",
			err:  fmt.Errorf("completions upstream rate limited: %s", "429 Too Many Requests"),
			want: http.StatusTooManyRequests,
		},
		{
			// 连接类错误没有状态码，保持 0（调用方按「无法解析→保守可重试」处理）
			name: "连接错误无状态码",
			err:  fmt.Errorf("upstream request failed: Post \"http://x/v1\": context canceled"),
			want: 0,
		},
		{
			name: "nil 错误",
			err:  nil,
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := upstreamStatusFromErr(tt.err); got != tt.want {
				t.Errorf("upstreamStatusFromErr(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

// 反向保护：修了 429 前缀后，确定性判定不能把 429 误当成确定性 4xx。
// 429 是 per-key 限流，应当跳过当前上游换下一个 key，而不是像 401/403 那样
// 「重试同一上游必然同样失败」。isDeterministicClientStatus 已经把 429 排除，
// 但前提是状态码能被正确解析出来——解析成 0 时属于「碰巧也对」，不可依赖。
func TestUpstreamStatusFromErr_RateLimitNotDeterministic(t *testing.T) {
	err := fmt.Errorf("upstream rate limited: %s", "429 Too Many Requests")
	status := upstreamStatusFromErr(err)
	if status != http.StatusTooManyRequests {
		t.Fatalf("429 状态码解析失败: got %d", status)
	}
	if isDeterministicClientStatus(status) {
		t.Error("429 不应被判为确定性 4xx（应跳过该 key 而非放弃重试）")
	}

	// 401 仍应被判为确定性 4xx：同一 key 重试必然同样失败
	if !isDeterministicClientStatus(http.StatusUnauthorized) {
		t.Error("401 应判为确定性 4xx")
	}
}
