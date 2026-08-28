// Package proxy reasoning_content 回传兼容：DeepSeek V4 系列（deepseek-v4-flash/pro）
// 默认开启 thinking 模式，多轮 Tool-Calling 时要求 assistant 消息的 reasoning_content
// 在后续请求中原样回传，否则上游返回 400。客户端 agent（pi / Claude Code / Hermes 等）
// 在消息规范化时可能丢掉该字段，网关在此做兼容层：
//   - 转发响应时解析 assistant 消息/delta 中的 reasoning_content，按 tool_call_id 缓存
//   - 转发请求前为丢失该字段的 assistant 消息自动补回（系统设置开关，默认开启）
package proxy

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/icefairy/xuanji/internal/store"
)

// defaultReasoningCacheMax 是内存侧 reasoning_content 缓存的最大条目数，超出后 FIFO 淘汰最旧。
// DB 侧无容量限制，仅按 ReasoningCacheRetainDays 清理过期记录。
const defaultReasoningCacheMax = 500

// ReasoningCache 是 reasoning_content 的缓存，key=tool_call_id。
// 双写设计：内存侧用于低延迟读/写，DB 侧用于跨重启持久化。
// 写入：同时写内存 + DB（DB 写失败不影响主流程，仅记日志）。
// 读取：先查内存，未命中再查 DB（DB 未命中返回 false，不重试内存）。
// 容量上限 defaultReasoningCacheMax（内存侧），DB 侧由 dailyStatsTicker 每日 prune。
type ReasoningCache struct {
	mu    sync.Mutex
	max   int
	items map[string]string // tool_call_id → reasoning_content
	order []string          // 插入顺序（FIFO 淘汰）
	db    *store.Store      // 持久化 backing；nil 时仅内存模式
}

// NewReasoningCache 创建 reasoning_content 缓存。max<=0 时用默认值 500。
// db 为可选的持久化 backing：非 nil 时写操作双写 DB，读操作内存未命中则 fallback DB。
func NewReasoningCache(max int, db *store.Store) *ReasoningCache {
	if max <= 0 {
		max = defaultReasoningCacheMax
	}
	return &ReasoningCache{max: max, items: make(map[string]string), db: db}
}

// Put 写入 tool_call_id 对应的 reasoning_content。已存在时更新值但保持原插入顺序
// （避免"热 key"把其他条目挤出）；空 id 或空内容不缓存（注入空串无意义）。
// 同时双写 DB（DB 写失败仅记日志，不影响主流程）。
func (c *ReasoningCache) Put(id, content string) {
	if id == "" || content == "" {
		return
	}
	c.mu.Lock()
	if _, exists := c.items[id]; exists {
		c.items[id] = content
		c.mu.Unlock()
	} else {
		c.items[id] = content
		c.order = append(c.order, id)
		for len(c.order) > c.max {
			oldest := c.order[0]
			c.order = c.order[1:]
			delete(c.items, oldest)
		}
		c.mu.Unlock()
	}
	// 双写 DB（异步感：不阻塞主流程，失败仅日志）
	if c.db != nil {
		if err := c.db.PutReasoning(id, content); err != nil {
			slog.Default().Debug("reasoning_cache: db write failed", "id", id, "error", err)
		}
	}
}

// PutAll 为多个 tool_call_id 写入同一份 reasoning_content
// （assistant 消息带多个并行 tool_calls 时，reasoning 是共享的）。
func (c *ReasoningCache) PutAll(ids []string, content string) {
	for _, id := range ids {
		c.Put(id, content)
	}
}

// Get 读取 tool_call_id 对应的 reasoning_content，未命中返回 false。
// 优先查内存；内存未命中时 fallback 查 DB。
func (c *ReasoningCache) Get(id string) (string, bool) {
	c.mu.Lock()
	v, ok := c.items[id]
	c.mu.Unlock()
	if ok {
		return v, true
	}
	// 内存未命中 → DB fallback
	if c.db != nil {
		if dbV, err := c.db.GetReasoning(id); err == nil && dbV != "" {
			// 回填内存（热路径加速后续读取）
			c.Put(id, dbV)
			return dbV, true
		}
	}
	return "", false
}

// Len 返回当前缓存条目数。
func (c *ReasoningCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// collectToolResultIDs 从 messages 中收集所有 tool 消息（role=tool）的 tool_call_id。
// 这些 id 代表"已经执行完、有结果"的工具调用。
func collectToolResultIDs(msgs []gjson.Result) map[string]bool {
	result := make(map[string]bool)
	for _, m := range msgs {
		if m.Get("role").String() != "tool" {
			continue
		}
		if id := m.Get("tool_call_id").String(); id != "" {
			result[id] = true
		}
	}
	return result
}

// collectAssistantToolCallIDs 从 messages 中收集所有 assistant 消息的 tool_calls[].id。
func collectAssistantToolCallIDs(msgs []gjson.Result) map[string]bool {
	result := make(map[string]bool)
	for _, m := range msgs {
		if m.Get("role").String() != "assistant" {
			continue
		}
		tcs := m.Get("tool_calls")
		if !tcs.IsArray() {
			continue
		}
		for _, tc := range tcs.Array() {
			if id := tc.Get("id").String(); id != "" {
				result[id] = true
			}
		}
	}
	return result
}

// shouldInjectReasoning 判断是否安全地为某条 assistant 消息注入 reasoning_content。
// 安全条件：该消息的所有 tool_call_id 在当前 messages 中都有对应的 tool_result。
// 原因：如果存在"新"的 tool_call（无对应 tool_result），注入 reasoning_content 会让上游
// 认为这是历史 context，导致消息链断裂（tool_call 找不到对应 tool_result）→ 400。
// 当某条消息同时包含"新"和"旧"的 tool_call 时，保守起见不注入（避免部分注入导致不一致）。
func shouldInjectReasoning(tcIDs []string, toolResultIDs map[string]bool) bool {
	if len(tcIDs) == 0 {
		return false
	}
	for _, id := range tcIDs {
		if id == "" {
			continue
		}
		if !toolResultIDs[id] {
			// 存在没有对应 tool_result 的 tool_call → 可能是"新"调用 → 不注入
			return false
		}
	}
	return true
}

// injectReasoningContent 在转发请求前为 messages 中丢失 reasoning_content 的
// assistant 消息补回缓存值：找到 role=assistant 且含 tool_calls 但无
// reasoning_content 字段的消息，用 tool_calls[].id 查缓存，命中则注入该字段。
// 只补 reasoning_content，不碰 role/content/tool_calls 等字段（零破坏）；
// 未命中跳过——尽力而为，不因缺缓存而失败。返回修改后的 body 与是否发生修改。
//
// 安全约束（避免上游 400）：
//   - 只在 tool_call_id 有对应 tool_result 时注入（说明是"已完成的旧调用"）
//   - 存在"新"的 tool_call（无 tool_result）时跳过注入，防止消息链断裂
//   - 这样 Hermes L3 折叠后孤立的 assistant+tool_calls 消息不会被误注入
func injectReasoningContent(body []byte, cache *ReasoningCache) ([]byte, bool) {
	if cache == nil {
		return body, false
	}
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return body, false
	}
	arr := msgs.Array()

	// 收集所有 tool 消息的 tool_call_id（已完成的工具调用）
	toolResultIDs := collectToolResultIDs(arr)
	_ = collectAssistantToolCallIDs(arr) // 保留函数以备扩展（当前仅用于记录）

	nb := body
	changed := false
	for i := range arr {
		m := arr[i]
		if m.Get("role").String() != "assistant" {
			continue
		}
		// 已有 reasoning_content（含显式空值）不回补，尊重客户端原样透传
		if m.Get("reasoning_content").Exists() {
			continue
		}
		tcs := m.Get("tool_calls")

		var tcIDs []string
		if tcs.IsArray() && len(tcs.Array()) > 0 {
			// 普通情况：assistant 消息含 tool_calls，用其 tool_call_id 查缓存
			for _, tc := range tcs.Array() {
				if id := tc.Get("id").String(); id != "" {
					tcIDs = append(tcIDs, id)
				}
			}
		} else {
			// Hermes L3 折叠场景：assistant 消息 content 为空且无 tool_calls 数组，
			// 但其后紧邻的 tool 消息携带 tool_call_id——说明该 assistant 曾经生成过 thinking
			// 且调用了工具，只是被折叠/规范化时丢掉了 tool_calls 字段。
			// 用下一个 tool 消息的 tool_call_id 反查缓存注入 reasoning_content。
			if m.Get("content").String() != "" {
				continue // content 非空，不是折叠后的思考消息，跳过
			}
			for j := i + 1; j < len(arr); j++ {
				tm := arr[j]
				if tm.Get("role").String() == "tool" {
					if id := tm.Get("tool_call_id").String(); id != "" {
						tcIDs = append(tcIDs, id)
					}
					break // 只取第一条紧邻的 tool 消息
				}
				// 遇到非 tool 消息则停止查找（说明没有紧邻的 tool 响应）
				break
			}
		}

		if len(tcIDs) == 0 {
			continue
		}
		// 安全检查：只有所有 tool_call_id 都有对应 tool_result 时才注入
		if !shouldInjectReasoning(tcIDs, toolResultIDs) {
			continue
		}
		// 任一 id 命中即注入同一份 reasoning（共享思考内容）
		for _, id := range tcIDs {
			if id == "" {
				continue
			}
			if rc, ok := cache.Get(id); ok && rc != "" {
				var err error
				nb, err = sjson.SetBytes(nb, fmt.Sprintf("messages.%d.reasoning_content", i), rc)
				if err != nil {
					continue
				}
				changed = true
				break
			}
		}
	}
	return nb, changed
}

// cacheReasoningFromMessage 从非流式响应体提取 assistant 消息的 reasoning_content
// 与 tool_calls[].id，写入缓存（供下一轮 tool-calling 请求回传）。
// DeepSeek 非流式响应：choices[0].message.reasoning_content + tool_calls[].id。
func cacheReasoningFromMessage(data []byte, cache *ReasoningCache) {
	if cache == nil {
		return
	}
	msg := gjson.GetBytes(data, "choices.0.message")
	if !msg.Exists() {
		return
	}
	rc := msg.Get("reasoning_content").String()
	if rc == "" {
		return
	}
	tcs := msg.Get("tool_calls")
	if !tcs.IsArray() || len(tcs.Array()) == 0 {
		return
	}
	var ids []string
	for _, tc := range tcs.Array() {
		if id := tc.Get("id").String(); id != "" {
			ids = append(ids, id)
		}
	}
	cache.PutAll(ids, rc)
}

// cacheReasoningDelta 从流式 SSE 的单个 data chunk 中提取 reasoning_content 分片与
// tool_call id：thinking 模式流式输出时 reasoning_content 是分片 delta，累积拼接到
// buf；tool_call id 出现的 chunk 到达时把当前已累积的 reasoning 立即写入缓存
// （此时 reasoning 通常已完整）。流结束（[DONE] / EOF）时由调用方用完整 buf 再写一次，
// 覆盖 tool_calls 之后才输出的分片。
func cacheReasoningDelta(data string, buf *strings.Builder, ids *[]string, cache *ReasoningCache) {
	if cache == nil {
		return
	}
	delta := gjson.Get(data, "choices.0.delta")
	if !delta.Exists() {
		return
	}
	if rc := delta.Get("reasoning_content").String(); rc != "" {
		buf.WriteString(rc)
	}
	tcs := delta.Get("tool_calls")
	if !tcs.IsArray() {
		return
	}
	for _, tc := range tcs.Array() {
		id := tc.Get("id").String()
		if id == "" {
			continue // arguments 增量 chunk 不带 id，跳过
		}
		*ids = append(*ids, id)
		if buf.Len() > 0 {
			cache.Put(id, buf.String())
		}
	}
}
