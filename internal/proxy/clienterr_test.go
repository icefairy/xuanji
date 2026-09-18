package proxy

import (
	"errors"
	"net/http"
	"testing"
)

// TestIsClientRequestError 验证「客户端请求问题」分类器：
// 模型名不存在/不支持、输入超长 → true（不记上游故障）；
// 鉴权、配额、5xx、连接类 → false（保留原有上游故障语义）。
func TestIsClientRequestError(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		// —— 客户端请求问题（true）——
		{
			name:   "wechat invalid model（2026-09-12 实测 150 次）",
			status: http.StatusBadRequest,
			body:   `{"error":{"message":"invalid model: model name not found","code":400}}`,
			want:   true,
		},
		{
			name:   "model hy3 is not supported",
			status: http.StatusBadRequest,
			body:   `{"type":"error","error":{"type":"ModelError","message":"Model hy3 is not supported"}}`,
			want:   true,
		},
		{
			name:   "输入超长 longer than the model's context length",
			status: http.StatusBadRequest,
			body:   `{"object":"error","message":"The input (277702 tokens) is longer than the model's context length (262144 tokens).","type":"BadRequestError","code":400}`,
			want:   true,
		},
		{
			name:   "输入超长 exceeds the model's maximum context length",
			status: http.StatusBadRequest,
			body:   `{"object":"error","message":"Requested token count exceeds the model's maximum context length of 262144 tokens.","type":"BadRequestError"}`,
			want:   true,
		},
		{
			name:   "input length too long",
			status: http.StatusBadRequest,
			body:   `{"code":11115,"msg":"input length too long","extError":{"code":"context_length_exceeded"}}`,
			want:   true,
		},
		{
			name:   "非 JSON 响应体但含特征",
			status: http.StatusBadRequest,
			body:   `upstream says: model not found`,
			want:   true,
		},
		{
			name:   "rerank 空 documents（2026-09-13 实测）",
			status: http.StatusBadRequest,
			body:   `{"code":20015,"message":"List should have at least 1 item after validation, not 0","data":null}`,
			want:   true,
		},
		{
			name:   "缺必填字段 Field required",
			status: http.StatusBadRequest,
			body:   `{"code":20015,"message":"Field required","data":null}`,
			want:   true,
		},
		{
			name:   "embeddings 空 input 数组（2026-09-13 实测）",
			status: http.StatusBadRequest,
			body:   `{"code":20015,"message":"The parameter is invalid. Please check again.","data":null}`,
			want:   true,
		},
		{
			name:   "rerank query 超长（2026-09-16 实测：query 5000 字符）",
			status: http.StatusBadRequest,
			body:   `{"code":20015,"message":"Query is too long. Please provide a shorter query.","data":null}`,
			want:   true,
		},
		{
			name:   "rerank query 为空串（同批 400 的另一形态）",
			status: http.StatusBadRequest,
			body:   `{"code":20015,"message":"String should have at least 1 character","data":null}`,
			want:   true,
		},

		// —— 非客户端请求问题（false，保持原有语义）——
		{
			name:   "401 鉴权失败（key 无效不会自愈）",
			status: http.StatusUnauthorized,
			body:   `{"error":{"message":"invalid api key"}}`,
			want:   false,
		},
		{
			name:   "402 欠费/订阅（上游账号状态）",
			status: http.StatusPaymentRequired,
			body:   `{"message":"subscription pre-verify unavailable"}`,
			want:   false,
		},
		{
			name:   "403 禁止访问",
			status: http.StatusForbidden,
			body:   `{"error":{"message":"forbidden"}}`,
			want:   false,
		},
		{
			name:   "429 限流（走 cooldown，非本分类器）",
			status: http.StatusTooManyRequests,
			body:   `{"error":{"message":"rate limit exceeded"}}`,
			want:   false,
		},
		{
			name:   "5xx 上游真实故障",
			status: http.StatusBadGateway,
			body:   `{"error":{"message":"bad gateway"}}`,
			want:   false,
		},
		{
			name:   "413 请求体过大（不可重试，走透传分支）",
			status: http.StatusRequestEntityTooLarge,
			body:   `<html><head><title>413 Request Entity Too Large</title></head></html>`,
			want:   false,
		},
		{
			name:   "400 但错误原因与请求无关（保留上游故障语义）",
			status: http.StatusBadRequest,
			body:   `{"error":{"message":"internal deserialization failure"}}`,
			want:   false,
		},
		{
			name:   "cfcdn rerank 端点不存在（No route for that URI，上游配置问题）",
			status: http.StatusBadRequest,
			body:   `{"success":false,"errors":[{"code":7000,"message":"No route for that URI"}]}`,
			want:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isClientRequestError(tt.status, []byte(tt.body)); got != tt.want {
				t.Errorf("isClientRequestError(%d, %s) = %v, want %v", tt.status, tt.body, got, tt.want)
			}
		})
	}
}

// TestClientRequestErrorSentinel 验证包装错误可通过 errors.Is 识别（调用方判定依据）。
func TestClientRequestErrorSentinel(t *testing.T) {
	wrapped := &clientRequestError{err: errors.New("upstream error: 400 Bad Request")}
	if !errors.Is(wrapped, errClientRequest) {
		t.Error("clientRequestError must match errClientRequest via errors.Is")
	}
	if wrapped.Error() != "upstream error: 400 Bad Request" {
		t.Errorf("Error() = %q, want %q（对外文本应保持原样）", wrapped.Error(), "upstream error: 400 Bad Request")
	}
	// 真实上游故障不应被误识别为客户端请求错误
	if errors.Is(errors.New("upstream error: 502 Bad Gateway"), errClientRequest) {
		t.Error("plain error must not match errClientRequest")
	}
}
