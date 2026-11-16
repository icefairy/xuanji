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
//   - codebuddy 回 `Model hy3 is not supported`
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
	// rerank query 超长（2026-09-16 实测，硅基流动 bge-reranker-v2-m3：
	// query 4000 字符 → 200，5000 字符 → 400 `Query is too long. Please provide a shorter
	// query.`）。同一超长 query 会沿候选链逐个 400，曾把三个健康 rerank 上游全部拉黑，
	// 属客户端请求内容问题而非上游故障。
	"query is too long",
	// 请求参数校验失败（客户端传了空数组/缺必填字段等；上游正确校验后拒绝）
	// 例（2026-09-13 实测，硅基流动 rerank/embeddings）：
	//   `List should have at least 1 item after validation, not 0`（documents/input 为空数组）
	//   `Field required`（缺 query/documents/input）
	//   `The parameter is invalid. Please check again.`（参数非法）
	"list should have at least 1 item after validation",
	"field required",
	"the parameter is invalid",
	// 客户端传了不存在的语音/音色（mimo TTS，2026-11-02 实测）：
	//   message="Param Incorrect"
	//   param="Unknown voice: female. Available voices: [mimo_default, 冰糖, …]"
	// 上游还很贴心地给了可用列表，说明它认得这个路径、只是客户端填错了名字。
	// 同一上游同日 4 次 200 / 2 次 400，且 400 后 1 秒内就有 200 → 上游健康。
	//
	// ⚠ 不能把 "param incorrect" 加进特征表：那是 mimo 的**通用错误外壳**
	// （同一条 message 也用于 param="unexpected server state" 这种上游真故障，
	// 见 clienterr_param_test.go 的反向保护用例）。只能按 param 的具体措辞匹配。
	"unknown voice",
	// 上游根本没有该模型的路由（unisound，2026-11-02 实测 6 次全 404）：
	//   {"error":{"message":"model route not found","type":"not_found_error",
	//             "param":null,"code":"model_not_found"}}
	// 触发场景：路由把 u2-asr / u2-tts 挂到了 /v1/chat/completions，而该上游没这两个 model。
	// 与已有的 model not found / does not exist 同性质，但措辞不同所以需单独列。
	"model route not found",
	// 入参形态/取值非法（pydantic 风格校验：query 为空串、query/documents 传了非字符串，
	// 2026-09-16 实测同上批 rerank 400）。同属客户端请求内容问题。
	"should have at least 1 character",
	"should be a valid string",
	// 请求形态与模型不匹配（2026-10-15 实测：客户端把 TTS 模型当 chat 模型用，
	// 发 messages 而非该模型要求的输入格式）。mimo 回 400：
	//   error.message="Param Incorrect"
	//   error.param="messages must contain an assistant role for TTS model"
	// 具体原因在 param 里（见 upstreamErrorText 的拼接）。同一上游换个合法请求
	// 即可成功（实测该模型走 /v1/audio/speech 路径 200），属客户端请求问题。
	"must contain an assistant role",
	// 消息结构非法（客户端会话里的 tool_call 缺 function.name，如空串名）。
	// 2026-10-28 实测 stepfun（11 次）：error.message="tool_calls.function.name is required"
	// type=input_invalid。11 次全部来自**同一个客户端连接**（mizar 会话），
	// 而同当日同 UA 有 206 次 200 → 上游健康，是那个会话的 tool_calls 坏了。
	// 不归入本表的后果：健康上游被 mark_failed + 打成 degraded/dead（5 次即 dead），
	// 相当于让一个坏会话把整条 stepfun 链路拉黑。
	"function.name is required",
	"function.name is missing",
	// 官方网关（workbuddy / codebuddy，同一套后端）对「本次请求参数被模型 provider 拒绝」
	// 固定回 code=11133，措辞有两版：
	//   "the request parameters were rejected by the model provider"（2026-10-29 实测 6 次）
	//   "Invalid request parameters"（2026-10-28 实测 hy3）
	// 关键：上游自己就把 extError.param 置空、Request/Response 也都是 null，说明它
	// 只到「模型层拒绝」这一层就返回了，没给字段级线索。因此**无法按 param 精确匹配**，
	// 只能按这句措辞归为客户端请求内容问题。
	//
	// 判据（与 stepfun 那一批同口径）：同期其他客户端 / 同客户端换会话均 200，
	// 上游当时在正常服务 → 换一个合法请求即可成功，属本次请求内容问题。
	// 不归入本表的后果：一个坏会话反复重试（客户端通常 2~3 次）累计到 5 次
	// 就把健康的 workbuddy / codebuddy 打成 dead，而它正在给别的 key 正常供数。
	//
	// 注意：不拿裸的 "invalid_request_error" 当特征——那是各家 4xx 的通用 type，
	// 会误伤鉴权/配额等其他错误（而且那些本应由上面的状态码开关先排掉）。
	"the request parameters were rejected by the model provider",
	"invalid request parameters",
	// code=11148（与 11133 同一套后端）：客户端会话的 tool_call / tool_result 配对不上
	// （assistant 带 tool_calls 但缺对应的 role=tool，或 tool 的 tool_call_id 对不上）。
	//
	// 证（三层，均为实测）：
	//   1) 上游自己的措辞就写着「please start a new conversation and retry」——
	//      说的是**这段会话坏了**，不是上游出问题；
	//   2) 2026-10-29 本地复现（本仓库）：assistant 带 tool_calls 而紧跟的 tool 无 id 配对，
	//      稳定得到与线上一致的 11148；
	//   3) 历史真实客户端 192.168.1.10 在 10-15/10-16 连续触发 18 次，每次 3 条一组
	//      （客户端自己的 2~3 轮重试），而同期 workbuddy 有 1125 次 200。
	//
	// 与 11133 同性质。不归入本表的后果：两个坏会话的重试即可累计 5 次
	// 把健康上游打成 dead。
	//
	// 注：2026-10-29 23:35 曾以本特征提交（9143ccf），1 分钟后被无说明 revert（898f816）。
	// 2026-10-30 复盘确认理由仍成立（上游措辞 + 本地复现 + 同期大量 200），故恢复。
	// 与拦截式修复（validateToolCallNames，23835d9）职责不同：那一条只能拦空函数名，
	// 配对错位/缺 tool 结果的会话仍需本特征兼底。
	"tool calls and tool results do not match",
	// agnes 系对同一个坏请求的措辞（2026-10-28 同批实测，用空名 tool_call 反向验证时得出）：
	//   messages[1]: missing field `name` at line 1 column 202
	// 同一个请求 body：stepfun 报 "tool_calls.function.name is required"，
	// agnes 报 pydantic/serde 风格的 missing field。两款措辞必须同时在表，
	// 否则请求在 stepfun 上正确地不计故障、却把 7 个 agnes 上游全打成 dead。
	"missing field `name`",
	"missing field \"name\"",
	// 讯飞星火 MaaS（2026-10-29 实测 spark 上游）缺必填字段的措辞：
	//   {"error":{"code":10005,"message":"missing required field input","type":"invalid_request_error"}}
	// 触发场景：embeddings 传空串 / 空数组。同一个上游换个合法 input 立刻 200，
	// 属客户端请求内容问题。
	// 不归入本表的后果：spark 是该 embedding 路由的唯一上游，一次空 input 就
	// mark_failed，连续 5 次打成 dead，而它一直在正常服务 spark-x2.6 对话。
	"missing required field",
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
//
// 2026-10-15 实测补充：部分上游把「泛化的摘要」放 message、把「具体原因」放 error.param，
// 例 mimo 对 TTS 模型收到 chat 格式请求时回：
//
//	{"error":{"code":"400","message":"Param Incorrect",
//	          "param":"messages must contain an assistant role for TTS model"}}
//
// 只取 message 会得到无信息量的 "Param Incorrect"，匹配不到任何特征，
// 使该类客户端请求问题被误计为上游故障（fastfail mark_failed + degraded）。
// 故把 error.param 一并拼入（不改各字段的优先级顺序）。
func upstreamErrorText(respBody []byte) string {
	for _, path := range []string{"error.message", "message", "msg", "detail"} {
		if s := gjson.GetBytes(respBody, path).String(); s != "" {
			// error.param 是同一错误对象内的补充说明，与 message 同源，拼上以覆盖具体原因。
			if p := gjson.GetBytes(respBody, "error.param").String(); p != "" {
				return s + " " + p
			}
			return s
		}
	}
	return string(respBody)
}
