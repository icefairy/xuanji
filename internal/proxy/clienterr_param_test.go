package proxy

import (
	"net/http"
	"strings"
	"testing"
)

// 本文件覆盖「上游把具体原因放在非 message 字段」时的客户端请求类判定。
//
// 背景（2026-10-15 实测）：客户端把 TTS 模型当 chat 模型用（发 messages 而非 input），
// mimo 回 400 合法拒绝，但原因写在 `error.param` 里，`error.message` 只有泛化的
// "Param Incorrect"：
//
//	{"error":{"code":"400","message":"Param Incorrect",
//	          "param":"messages must contain an assistant role for TTS model"}}
//
// upstreamErrorText 只取 error.message/message/msg/detail，拿到的是无信息量的
// "Param Incorrect"，匹配不到任何特征 → 该 400 被当成上游故障：
//
//	WARN fastfail mark_failed upstream=mimo reason="status=400 body=..."
//	WARN upstream failed during proxy forward upstream=mimo state=degraded fails=1
//
// 这是 2026-09-12/09-13 同一缺陷家族的第 5 次变体（客户端请求类 4xx 误计上游故障）。
// 修复：upstreamErrorText 把 `error.param` 也纳入提取（与 message 拼接），
// 使具体原因参与特征匹配。

// TestIsClientRequestError_ReasonInErrorParam 验证原因位于 error.param 时也能正确分类。
func TestIsClientRequestError_ReasonInErrorParam(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{
			name:   "mimo TTS 当 chat 用：原因在 error.param（2026-10-15 实测）",
			status: http.StatusBadRequest,
			body: `{"error":{"code":"400","message":"Param Incorrect",` +
				`"param":"messages must contain an assistant role for TTS model","type":""}}`,
			want: true,
		},
		{
			name:   "原因在 error.param：模型不存在",
			status: http.StatusBadRequest,
			body: `{"error":{"message":"Param Incorrect",` +
				`"param":"model does not exist in this region"}}`,
			want: true,
		},
		{
			name:   "原因在 error.param：参数校验失败",
			status: http.StatusBadRequest,
			body: `{"error":{"message":"Param Incorrect",` +
				`"param":"the parameter is invalid, please check again"}}`,
			want: true,
		},
		// —— 反向保护：不得因纳入 param 而把上游真实故障误判为客户端问题 ——
		{
			name:   "message 与 param 均无特征（保留上游故障语义）",
			status: http.StatusBadRequest,
			body:   `{"error":{"message":"Param Incorrect","param":"unexpected server state"}}`,
			want:   false,
		},
		{
			name:   "401 鉴权失败即使 param 含特征也不放过",
			status: http.StatusUnauthorized,
			body:   `{"error":{"message":"invalid api key","param":"model not found"}}`,
			want:   false,
		},
		{
			name:   "403 禁止访问不因 param 特征而放过",
			status: http.StatusForbidden,
			body:   `{"error":{"message":"forbidden","param":"model not found"}}`,
			want:   false,
		},
		{
			name:   "5xx 上游真实故障不受 param 影响",
			status: http.StatusBadGateway,
			body:   `{"error":{"message":"bad gateway","param":"model not found"}}`,
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

// TestUpstreamErrorText_IncludesParam 直接锁定提取口径：
// error.message 与 error.param 都要参与，且不得丢掉 message 本身。
func TestUpstreamErrorText_IncludesParam(t *testing.T) {
	body := []byte(`{"error":{"message":"Param Incorrect",` +
		`"param":"messages must contain an assistant role for TTS model"}}`)
	got := strings.ToLower(upstreamErrorText(body))
	if !strings.Contains(got, "param incorrect") {
		t.Errorf("upstreamErrorText 应保留 error.message，got %q", got)
	}
	if !strings.Contains(got, "must contain an assistant role") {
		t.Errorf("upstreamErrorText 应包含 error.param 的具体原因，got %q", got)
	}
}
