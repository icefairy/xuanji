package proxy

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/debuglog"
	"github.com/tidwall/gjson"
)

// 本文件实现「异常请求完整诊断日志」（系统设置开关 proxy.debug_request_log）。
//
// 与 request_log 的分工：
//   - request_log：长期结构化指标（不含请求体），用于统计与快速排查；
//   - 本日志：**短时排障**用的完整报文快照（含完整提示词 + 该候选实际发出的请求体），
//     NDJSON 一行一条，落到 <数据库目录>/debug-logs/，默认关闭、保留 7 天。
//
// 设计取向：
//   - writer 可热替换（atomic.Pointer），nil/disabled 时 measureXXX 直接返回零值，
//     不复制请求体、不做任何字符串转换；
//   - 每个异常候选各落一条记录（中间候选 final=false，最终候选 final=true），
//     因此「先失败后切换成功」的场景仍保留中间候选的异常记录；
//   - 绝不写请求头（Authorization / Cookie / X-Api-Key 一律不落盘）。

// debugAttempt 是从请求入口传到单个候选转发函数的日志上下文。
//
// 为什么单独一个结构体：forwardOnce 的参数已经很多，再加 5 个散参数会让所有
// 调用点（含 20+ 测试）变得不可读；这里只加一个可 nil 的指针参数。
type debugAttempt struct {
	// RequestID 本次客户端请求的关联 id（同一次请求的所有候选共用一个值）。
	// 取客户端 X-Request-Id，缺失时本地生成——方便把多候选记录按请求串起来。
	RequestID string
	// Endpoint 入口标识：chat / rerank / embedding。
	Endpoint string
	// RawClientBody 是**客户端原始请求体**（原字节，未经网关任何改写），
	// 记录到 raw_request 字段，用于和 upstream_request 对照「网关发了什么」。
	RawClientBody []byte
	// attempts 本请求已发生的候选尝试数（含同一上游的重试），用于 attempt 字段。
	attempts int
	// recs 本次请求已发生的异常候选记录，请求结束时统一判定 final 并落盘。
	//
	// 为什么不在这里就写：只有回到调用方、知道「是否还会重试/客户端最终看到什么」
	// 之后，final 与 final_status 才可能是准确的。提前写会让每个重试尝试都自称 final，
	// 且 final_status 只能瞎填。
	recs []*debugRecord
	// ClientModel 客户端请求里的模型名（聚合模型名，非上游真实模型）。
	ClientModel string
	// Stream 客户端是否请求流式。
	Stream bool
}

// nextAttempt 返回本次尝试的序号（1 起，跨同一上游的重试递增）。
func (a *debugAttempt) nextAttempt() int {
	a.attempts++
	return a.attempts
}

// debugTarget 是当前生效的日志目标（writer + 开关），整体替换以保证热重载原子性。
type debugTarget struct {
	writer *debuglog.Writer
}

// SetDebugLog 替换调试日志 writer（nil = 关闭）。热重载时调用。
//
// 注意：不在这里 Close 旧 writer——旧 writer 可能仍有在途请求在写
// （reload 期间请求用旧配置跑完）。关闭旧文件的职责在 main 的
// reload 流程里（比对后 defer close），这里只做原子替换。
func (h *Handler) SetDebugLog(w *debuglog.Writer) {
	if w == nil {
		h.debugLog.Store(nil)
		return
	}
	h.debugLog.Store(&debugTarget{writer: w})
}

// debugEnabled 是启动/热重载时调用一次的成功路径快路径判断。
// 直接用 cfg 而非 writer：writer 是惰性创建的，可能尚未建立。
func (h *Handler) debugEnabled() bool {
	return h.cfg != nil && h.cfg.Proxy.DebugRequestLog
}

// debugReady 返回可用的 writer；未开启/未注入时返回 nil（调用方必须早退）。
func (h *Handler) debugReady() *debuglog.Writer {
	t := h.debugLog.Load()
	if t == nil {
		return nil
	}
	return t.writer
}

// measureDebugBody 在入口读完全部 body 后调用：开关开启时**复制**原始请求体
// 供异常时落盘；关闭时直接返回（不拷贝、不分配）。
//
// 为什么必须复制：body 在转发链路上会被多次 sjson 改写（模型名、max_tokens、
// 思考参数……），而日志要记录的是「客户端原本发了什么」——只有入口这几行之前的
// 字节是原样的。关闭开关时为零开销。
func (h *Handler) measureDebugBody(att *debugAttempt, body []byte) {
	if att == nil || att.RawClientBody != nil {
		return
	}
	if !h.debugEnabled() {
		return
	}
	cp := make([]byte, len(body))
	copy(cp, body)
	att.RawClientBody = cp
}

// newDebugAttempt 在请求入口构造日志上下文（不复制 body，延迟到 measure）。
// requestID 取客户端 X-Request-Id，缺失时用纳秒时间戳 + 计数器生成。
func newDebugAttempt(r *http.Request, endpoint string) *debugAttempt {
	if r == nil {
		return nil
	}
	id := r.Header.Get("X-Request-Id")
	if id == "" {
		id = "xj-" + time.Now().Format("20060102T150405.000000000") + "-" + strconv.FormatInt(debugReqSeq.Add(1), 10)
	}
	return &debugAttempt{RequestID: id, Endpoint: endpoint}
}

var debugReqSeq atomic.Int64

// debugUpstreamRequest 是候选实际发往上游的请求体快照（延迟取得）。
//
// 为什么用闭包而不是直接传 []byte：上游请求体在 forwardOnce 里经过多道改写，
// 只有真正发请求那一刻才是最终值；而且只在异常时才有价值——
// 关闭开关或请求成功时给闭包等于零拷贝。
type debugUpstreamRequest struct {
	get func() []byte
}

func snapshotDebugReq(get func() []byte) *debugUpstreamRequest {
	if get == nil {
		return nil
	}
	return &debugUpstreamRequest{get: get}
}

// recordDebugCandidate 为单个异常候选**攒一条**记录（真正落盘在 requestScoped 结束时）。
//
// upStatus 是上游看到的真实状态码（连接失败记 0）；finalStatus 是下游最终
// 响应的归一化状态（流中断 499 / 全失败 502 等），两者都必须保留——
// 它们回答的是不同问题：「上游说了什么」 vs 「客户端最后看到什么」。
//
// 为什么不在候选失败当下就写文件：final 与 final_status 取决于**后续**是否还有
// 候选、是否重试、客户端最终看到什么。先攒后写才能在请求结束时给出准确终态
// （也避免同一候选的多次重试在日志里各自自称 final）。
//
// clientBody 是入口复制的原始客户端请求体；att 为 nil（未开启）时立即返回。
func (h *Handler) recordDebugCandidate(
	r *http.Request,
	att *debugAttempt,
	up *config.Upstream,
	upstreamModel string,
	upStatus, finalStatus int,
	ferr error,
	upReq *debugUpstreamRequest,
	clientBody []byte,
) {
	if att == nil || h.debugReady() == nil {
		return
	}
	rec := newDebugRecord(att, up, upstreamModel, upStatus, finalStatus, ferr)
	rec.RawRequest = rawClientBodyValue(clientBody)
	if upReq != nil && upReq.get != nil {
		rec.UpstreamRequest = jsonBodyValue(upReq.get())
	}
	att.recs = append(att.recs, rec)
}

// flushDebug 在请求结束时统一落盘：只有最后一次尝试异常时标记 final 并保留
// final_status；之前的候选一律 final=false + final_status=0（它们之后再换过上游）。
//
// 为什么「只有最后一条」：成功切换后，前面失败的候选只是过程；最终失败时，
// 最后一条才是决定客户端结果的那次（前面的都已被后续候选替代）。
// 若客户端最终成功（finalOK=true），则所有记录都不是终态，final_status 全部为 0。
func (h *Handler) flushDebug(att *debugAttempt, finalOK bool) {
	if att == nil || len(att.recs) == 0 {
		return
	}
	w := h.debugReady()
	if w == nil {
		return
	}
	if !finalOK {
		last := att.recs[len(att.recs)-1]
		last.Final = true
		// 下游最终归一化为 502（全部候选失败）：resolved 里可能没写 final_status
		if last.FinalStatus == 0 {
			last.FinalStatus = http.StatusBadGateway
		}
	}
	for _, rec := range att.recs {
		if err := w.Write(rec); err != nil && w.WarnOnce() {
			h.log.Warn("写入调试日志失败（后续不再重复提示）", "dir", w.Dir(), "error", err)
		}
	}
	att.recs = nil
}

// debugRecord 是一条异常候选记录（NDJSON 一行）。
//
// 字段名固定，避免后续排障脚本因改名失效；raw_request / upstream_request 用
// json.RawMessage 承载**原字节语义**：合法 JSON 直接内插（不转义成字符串），
// 非法输入退化为字符串，保证整行仍是合法 JSON。
type debugRecord struct {
	Timestamp       string          `json:"timestamp"`
	RequestID       string          `json:"request_id"`
	Endpoint        string          `json:"endpoint"`
	ClientModel     string          `json:"client_model"`
	Upstream        string          `json:"upstream"`
	UpstreamModel   string          `json:"upstream_model"`
	UpstreamStatus  int             `json:"upstream_status"`
	FinalStatus     int             `json:"final_status"`
	Final           bool            `json:"final"`
	Attempt         int             `json:"attempt"`
	Stream          bool            `json:"stream"`
	Error           string          `json:"error"`
	RawRequest      json.RawMessage `json:"raw_request"`
	UpstreamRequest json.RawMessage `json:"upstream_request,omitempty"`
}

func newDebugRecord(att *debugAttempt, up *config.Upstream, upstreamModel string,
	upStatus, finalStatus int, ferr error) *debugRecord {
	rec := &debugRecord{
		Timestamp:      time.Now().Format(time.RFC3339Nano),
		RequestID:      att.RequestID,
		Endpoint:       att.Endpoint,
		ClientModel:    att.ClientModel,
		UpstreamModel:  upstreamModel,
		UpstreamStatus: upStatus,
		FinalStatus:    finalStatus,
		Final:          false,
		Attempt:        att.nextAttempt(),
		Stream:         att.Stream,
	}
	if up != nil {
		rec.Upstream = up.Name
	}
	if ferr != nil {
		rec.Error = ferr.Error()
	}
	return rec
}

// rawClientBodyValue 返回原始客户端请求体（合法 JSON 直接内插，否则编码成字符串）。
// body 为空时写空字符串而非省略字段：排障时要能区分「没抓到」与「请求体本来为空」。
func rawClientBodyValue(body []byte) json.RawMessage {
	if len(body) == 0 {
		return json.RawMessage(`""`)
	}
	return jsonBodyValue(body)
}

// jsonBodyValue 把可能非法的请求体安全编码成 RawMessage。
func jsonBodyValue(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	if gjson.ValidBytes(b) {
		// 拷贝一份：原始切片可能被调用方复用/改写。
		cp := make([]byte, len(b))
		copy(cp, b)
		return json.RawMessage(cp)
	}
	// 非法 JSON（截断/非 UTF-8 字节）：编码为 JSON 字符串，长度与内容保持原样。
	enc, err := json.Marshal(string(b))
	if err != nil {
		return nil
	}
	return json.RawMessage(enc)
}
