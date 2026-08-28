// Package proxy 模型 token 上限自动学习与 clamp 兼容。
//
// 背景：客户端 agent（WorkBuddy、Cursor、JetBrains 等）按模型"窗口大小"自动填
// max_tokens / max_completion_tokens，但各上游允许的最大 completion token 数
// 往往小于模型窗口（如 deepseek-v4-flash 报 "at most 262144"）。传超大值上游
// 直接 400 "max_completion_tokens is too large: N, this model supports at most M"，
// 导致同一请求在多个上游间反复失败。
//
// 方案（两层兼容）：
//  1. [maxtokens.go] 无上限知识时用保守默认 cap（65536）做兜底 clamp。
//  2. [本文件] 从上游 400 错误 message 自动提取 "at most M" 写入 model_token_limits
//     表（按 上游名+真实模型名 唯一），下次同一模型请求自动 clamp 到 M。
//     已知模型（deepseek-v4-*）在 SeedDefaults 预置，无需先踩一次 400。
//
// 只 clamp 数值字段（max_tokens / max_completion_tokens）。若输入 messages 本身
// 超过上下文长度（错误非 "is too large" 类），clamp 无效，仍透传上游 400，
// 让客户端知道自己内容超限。
package proxy

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/store"
)

// tokenLimitTooLargeRe 匹配 "max_completion_tokens is too large: 963669. This model supports at most 262144"
// 或 "max_tokens is too large ... maximum ..." 类错误，提取两个数值：
//   - $1 字段名（max_completion_tokens / max_tokens）
//   - $2 模型允许的最大 completion tokens（要学习的值）
// 注意：不能匹配 "maximum context length ... your messages resulted in N" 类输入超长错误
// （该类错误无 "too large" 字样）—— 输入超长应透传 400 让客户端自省。
var tokenLimitTooLargeRe = regexp.MustCompile(`(?i)(max_completion_tokens|max_tokens).{0,120}?too large.{0,120}?(?:at most|maximum)[^0-9]*(\d+)`)

// extractMaxFromError 从上游错误响应提取"模型允许的最大 completion tokens"。
// 返回提取到的上限（>0 表示命中）与命中的字段名（max_completion_tokens / max_tokens）。
func extractMaxFromError(respBody []byte) (int, string) {
	if !gjson.ValidBytes(respBody) {
		return 0, ""
	}
	msg := gjson.GetBytes(respBody, "error.message").String()
	if msg == "" {
		msg = string(respBody)
	}
	msg = strings.Join(strings.Fields(msg), " ")
	m := tokenLimitTooLargeRe.FindStringSubmatch(msg)
	if len(m) != 3 {
		return 0, ""
	}
	n, err := strconv.Atoi(m[2])
	if err != nil || n <= 0 {
		return 0, ""
	}
	switch {
	case strings.Contains(m[1], "completion"):
		return n, "max_completion_tokens"
	case strings.Contains(m[1], "max_tokens"):
		return n, "max_tokens"
	default:
		return 0, ""
	}
}

// learnTokenLimit 从上游 400 响应学习模型 token 上限并写库。
// upstreamModel 为路由后的真实模型名（优先），为空时用客户端模型名兜底。
// 失败（无 store、未命中模式、无上限数据）时静默返回，不影响主流程。
func (h *Handler) learnTokenLimit(up *config.Upstream, upstreamModel, clientModel string, respBody []byte) {
	if h.tokenLimits == nil {
		return
	}
	max, field := extractMaxFromError(respBody)
	if max <= 0 {
		return
	}
	model := upstreamModel
	if model == "" {
		model = clientModel
	}
	if model == "" {
		return
	}
	var maxCompletion, maxTokens int
	switch field {
	case "max_completion_tokens":
		maxCompletion = max
	case "max_tokens":
		maxTokens = max
	default:
		return
	}
	if err := h.tokenLimits.UpsertTokenLimit(up.Name, model, maxCompletion, maxTokens, "error_message"); err != nil {
		h.log.Debug("learn token limit: db write failed", "upstream", up.Name, "model", model, "error", err)
		return
	}
	h.log.Info("learned model token limit from upstream error",
		"upstream", up.Name, "upstream_model", model,
		"max_completion_tokens", maxCompletion, "max_tokens", maxTokens)
}

// applyTokenLimit 转发前 clamp 请求体的 max_tokens / max_completion_tokens：
// 查 model_token_limits 表（精确上游 → 通配上游，见 Store.GetTokenLimit），
// 值超过记录上限则 clamp。未记录（首次请求或已被管理员删除）时不动 body，
// 由 maxtokens.go 的默认 cap 兜底。返回修改后的 body 与是否发生修改。
func (h *Handler) applyTokenLimit(body []byte, up *config.Upstream, upstreamModel string) ([]byte, bool) {
	lim := h.tokenLimits
	if lim == nil {
		return body, false
	}
	model := upstreamModel
	if model == "" {
		model = gjson.GetBytes(body, "model").String()
	}
	if model == "" {
		return body, false
	}
	row, err := lim.GetTokenLimit(up.Name, model)
	if err != nil || row == nil {
		return body, false
	}
	nb := body
	changed := false
	if row.MaxCompletionTokens > 0 {
		if v := gjson.GetBytes(nb, "max_completion_tokens").Int(); v > int64(row.MaxCompletionTokens) {
			if n, serr := sjson.SetBytes(nb, "max_completion_tokens", row.MaxCompletionTokens); serr == nil {
				nb = n
				changed = true
			}
		}
	}
	if row.MaxTokens > 0 {
		if v := gjson.GetBytes(nb, "max_tokens").Int(); v > int64(row.MaxTokens) {
			if n, serr := sjson.SetBytes(nb, "max_tokens", row.MaxTokens); serr == nil {
				nb = n
				changed = true
			}
		}
	}
	return nb, changed
}

// TokenLimitStore 返回当前 token limit 存储（admin API 用；nil 时表示未启用）。
func (h *Handler) TokenLimitStore() *store.Store {
	return h.tokenLimits
}

// SetTokenLimits 注入 token 上限学习的持久化存储（nil 安全：未注入时仅默认 cap 兜底）。
func (h *Handler) SetTokenLimits(s *store.Store) {
	h.tokenLimits = s
}