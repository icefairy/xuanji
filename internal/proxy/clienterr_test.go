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
			name:   "codebuddy Model hy3 is not supported",
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
			name:   "codebuddy input length too long",
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
		{
			// 同一个坏请求（messages 里 tool_call 缺 name）在 agnes 系的措辞不同。
			// 2026-10-28 部署后用空名 tool_call 反向验证时实测：
			// stepfun 报 "tool_calls.function.name is required"，
			// agnes 报 "messages[1]: missing field `name`"（serde 风格）。
			// 只覆盖前者的后果：一个坏请求把 7 个 agnes 上游全打成 dead。
			name:   "agnes 同一坏请求的 serde 措辞 missing field name（2026-10-28 实测）",
			status: http.StatusBadRequest,
			body:   "{\"error\":{\"message\":\"***.BadRequestError: OpenAIException - {\\\"error\\\":{\\\"message\\\":\\\"Invalid JSON data: Failed to deserialize the JSON body into the target type: messages[1]: missing field `name` at line 1 column 202\\\",\\\"type\\\":\\\"invalid_request_error\\\",\\\"code\\\":\\\"json_parse_error\\\"}}\",\"type\":\"invalid_request_error\"}}",
			want:   true,
		},
		{
			// 2026-11-02 实测 mimo TTS：客户端传了不存在的音色名，上游 400 并回
			// param="Unknown voice: female. Available voices: [mimo_default, 冰糖, ...]"。
			// 同一上游同日 4 次 200 / 2 次 400，且 400 后 1 秒内就说 200 → 上游健康。
			// 不归入本表的后果：客户端填错一个音色名，就能把能正常服务的 mimo 计入故障。
			name:   "mimo TTS 不存在的话音名 Unknown voice（2026-11-02 实测）",
			status: http.StatusBadRequest,
			body:   `{"error":{"code":"400","message":"Param Incorrect","param":"Unknown voice: female. Available voices: [mimo_default, 冰糖, 茉莉, 苏打, 白桦, Mia, Chloe, Milo, Dean]","type":""}}`,
			want:   true,
		},
		{
			// 2026-11-02 实测 unisound：客户端把 u2-asr / u2-tts 发到 /v1/chat/completions，
			// 上游根本没有这些 model route（6 次全部 404，从未成功过）。
			// 证：上游只在“这个模型路由不存在”时回 model route not found，
			// 与形态“model not found / does not exist”同性质。
			name:   "unisound model route not found（2026-11-02 实测）",
			status: http.StatusNotFound,
			body:   `{"error":{"message":"model route not found","type":"not_found_error","param":null,"code":"model_not_found"}}`,
			want:   true,
		},
		{
			// 2026-10-28 实测 stepfun（step-3.7-flash，11 次）：
			// 客户端会话里有空名 tool_call，上游正确拒绝。同一上游同日 499 次 200，
			// 换个合法请求即可成功 → 属客户端请求内容问题，不该打上游健康。
			name:   "stepfun tool_calls.function.name is required（2026-10-28 实测）",
			status: http.StatusBadRequest,
			body:   `{"error":{"message":"tool_calls.function.name is required","type":"input_invalid"}}`,
			want:   true,
		},
		{
			// 2026-10-29 实测 workbuddy（deepseek-v4-flash，6 次）：官方网关回
			// code=11133 「参数被模型 provider 拒绝」。判据：
			//   - 同一上游同一时刻同模型的其他请求全部 200（piagent 19:26:24/27/29 三次 200）
			//   - 同一客户端之后换个会话又全部 200（mizar 19:27 起连续 200）
			//   - 上游自己在 displayTips 里写明「这次对话的内容这个模型不接受，与网络无关」
			// 即换一个合法会话即可成功，上游本身健康。
			// 不归入本表的后果：一个坏会话反复重试，5 次就能把健康的 workbuddy 打成 dead，
			// 而它当时正给别的客户端正常服务。
			name:   "workbuddy 11133 参数被模型拒绝（2026-10-29 实测）",
			status: http.StatusBadRequest,
			body:   `{"code":11133,"msg":"the request parameters were rejected by the model provider","requestId":"01a18774dae97591b1766ff940992fd5","extError":{"code":"invalid_request_error","message":"the request parameters were rejected by the model provider","param":"","type":"invalid_request_error","StatusCode":400}}`,
			want:   true,
		},
		{
			// code=11148：客户端会话的 tool_call / tool_result 配对不上（如 assistant
			// 带 tool_calls 而缺对应的 role=tool，或 tool_call_id 对不上）。
			// 证：历史上真实客户端 192.168.1.10 在 2026-10-15 / 10-16 连续触发（每次 3 条一组，
			// 即客户端自己的 2~3 次重试），而同一天 workbuddy 有 1125 次 200。
			// 2026-10-29 本地也复现过同一形态（assistant 带 tool_calls 而 tool 无配对）。
			// 不归入本表的后果：一个坏会话的 2~3 次重试 + 另一个坏会话就能累计到 5 次，
			// 把健康的 workbuddy 打成 dead。
			name:   "workbuddy 11148 tool calls 与 tool results 不匹配（2026-10-29 实测）",
			status: http.StatusBadRequest,
			body:   `{"code":11148,"msg":"tool calls and tool results do not match, please start a new conversation and retry","requestId":"01a1877bcb3176e79d31488bde933d71","extError":{"code":"invalid_request_error","message":"tool calls and tool results do not match, please start a new conversation and retry","param":"","type":"invalid_request_error","StatusCode":400}}`,
			want:   true,
		},
		{
			// codebuddy / workbuddy 同源后端对非法参数的另一种措辞（2026-10-28 实测 hy3）。
			// 与 11133 同一性质：上游正确校验后拒绝本次请求，换请求即可成功。
			name:   "codebuddy 11133 Invalid request parameters（2026-10-28 实测）",
			status: http.StatusBadRequest,
			body:   `{"code":11133,"msg":"Invalid request parameters","requestId":"01a180e0-d85f-7346-8b3a-de822ecac193","extError":{"code":"invalid_request_error"}}`,
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
