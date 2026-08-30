// Package proxy reasoning_content 回传兼容：DeepSeek V4 系列（deepseek-v4-flash/pro）
// 默认开启 thinking 模式，多轮 Tool-Calling 时要求 assistant 消息的 reasoning_content
// 在后续请求中原样回传，否则上游返回 400。
//
// 设计要点：
//   - 按 tool_call_id 精确匹配（assistant 消息带 tool_calls 的轮次）
//   - 按 content 指纹（FNV-1a 64-bit）匹配（assistant 消息纯思考回答的轮次）
//   - 双写：内存（低延迟）+ SQLite（跨重启持久化）
//   - 指纹覆盖 DeepSeek 官方要求：带 tools 参数时所有历史轮次（含无 tool_calls）的
//     reasoning_content 都必须回传（https://api-docs.deepseek.com/guides/thinking_mode/）
package proxy

import (
	"fmt"
	"hash/fnv"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/icefairy/xuanji/internal/store"
)

// defaultReasoningCacheMax 是内存侧 reasoning_content 缓存的最大条目数，超出后 FIFO 淘汰最旧。
// DB 侧无容量限制，仅按 ReasoningCacheRetainDays 清理过期记录。
const defaultReasoningCacheMax = 500

// ReasoningCache 是 reasoning_content 的缓存，支持两种 key：
//   - tool_call_id（带 tool_calls 的 assistant 消息）
//   - content 指纹（无 tool_calls 的纯思考 assistant 消息，整条 content 的 FNV-1a 64-bit hash）
//
// 双写设计：内存侧用于低延迟读/写，DB 侧用于跨重启持久化。
// 写入：同时写内存 + DB。读取：先查内存，未命中再查 DB 并回填热缓存。
// 容量上限 defaultReasoningCacheMax（内存侧），DB 侧由 dailyStatsTicker 每日 prune 7 天前记录。
type ReasoningCache struct {
	mu      sync.Mutex
	max     int
	items   map[string]string // key=tool_call_id, value=reasoning_content
	order   []string          // 插入顺序（FIFO 淘汰）
	fprints map[string]string // key=content指纹, value=reasoning_content
	fOrder  []string          // 插入顺序（FIFO 淘汰）
	db      *store.Store      // 持久化 backing；nil 时仅内存模式
}

// NewReasoningCache 创建 reasoning_content 缓存。max<=0 时用默认值 500。
// db 为可选的持久化 backing：非 nil 时写操作双写 DB，读操作内存未命中则 fallback DB。
func NewReasoningCache(max int, db *store.Store) *ReasoningCache {
	if max <= 0 {
		max = defaultReasoningCacheMax
	}
	return &ReasoningCache{
		max:     max,
		items:   make(map[string]string),
		fprints: make(map[string]string),
		db:      db,
	}
}

// Put 写入 tool_call_id 对应的 reasoning_content。已存在时更新值但保持原插入顺序
// （避免"热 key"把其他条目挤出）。空 id 拦截；content 允许空串——"该轮无思考"
// 也是有效状态：DeepSeek thinking 模式要求 assistant(tool_calls) 必须回传
// reasoning_content（空串也算已回传），空串不缓存会导致下一轮注入失败 → 400。
// 同时双写 DB（DB 键前缀 "tc:"）。
func (c *ReasoningCache) Put(id, content string) {
	if id == "" {
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
	if c.db != nil {
		if err := c.db.PutReasoning("tc:"+id, content); err != nil {
			slog.Default().Debug("reasoning_cache: db write failed", "key", "tc:"+id, "error", err)
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

// PutFingerprint 按 content 全文指纹写入 reasoning_content。
// 覆盖无 tool_calls 的纯思考回答轮次，FIFO 淘汰同内存容量。
// DB 键前缀 "fp:"（含 64-bit FNV-1a hex hash）。
func (c *ReasoningCache) PutFingerprint(content, reasoning string) {
	if content == "" || reasoning == "" {
		return
	}
	fp := contentFingerprint(content)
	c.mu.Lock()
	if _, exists := c.fprints[fp]; exists {
		c.fprints[fp] = reasoning
		c.mu.Unlock()
	} else {
		c.fprints[fp] = reasoning
		c.fOrder = append(c.fOrder, fp)
		for len(c.fOrder) > c.max {
			oldest := c.fOrder[0]
			c.fOrder = c.fOrder[1:]
			delete(c.fprints, oldest)
		}
		c.mu.Unlock()
	}
	if c.db != nil {
		if err := c.db.PutReasoning("fp:"+fp, reasoning); err != nil {
			slog.Default().Debug("reasoning_cache: db fp write failed", "key", "fp:"+fp, "error", err)
		}
	}
}

// Get 读取 tool_call_id 对应的 reasoning_content，未命中返回 false。
// 优先查内存；内存未命中时 fallback 查 DB（键前缀 "tc:"）。
// 命中值可为空串（"该轮无思考"标记），err == nil 即命中。
func (c *ReasoningCache) Get(id string) (string, bool) {
	c.mu.Lock()
	v, ok := c.items[id]
	c.mu.Unlock()
	if ok {
		return v, true
	}
	if c.db != nil {
		if dbV, err := c.db.GetReasoning("tc:" + id); err == nil {
			c.Put(id, dbV)
			return dbV, true
		}
	}
	return "", false
}

// GetByFingerprint 按 content 全文指纹查找 reasoning_content，未命中返回 false。
// 优先查内存；内存未命中时 fallback 查 DB（键前缀 "fp:"）。
func (c *ReasoningCache) GetByFingerprint(content string) (string, bool) {
	if content == "" {
		return "", false
	}
	fp := contentFingerprint(content)
	c.mu.Lock()
	v, ok := c.fprints[fp]
	c.mu.Unlock()
	if ok {
		return v, true
	}
	if c.db != nil {
		if dbV, err := c.db.GetReasoning("fp:" + fp); err == nil && dbV != "" {
			c.PutFingerprint(content, dbV)
			return dbV, true
		}
	}
	return "", false
}

// Len 返回 tool_call 缓存条目数（用于测试）。
func (c *ReasoningCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// contentFingerprint 计算 content 字符串的 FNV-1a 64-bit hex 指纹。
// 碰撞概率约 2^-64，非对抗性场景完全够用。
func contentFingerprint(content string) string {
	h := fnv.New64a()
	h.Write([]byte(content))
	return strconv.FormatUint(h.Sum64(), 16)
}

// ===== 消息注入逻辑 =====

// collectToolResultIDs 从 messages 中收集所有 tool 消息（role=tool）的 tool_call_id。
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

// shouldInjectReasoning 判断是否安全地为某条 assistant 消息注入 reasoning_content。
// 安全条件：该消息的所有 tool_call_id 在当前 messages 中都有对应的 tool_result。
func shouldInjectReasoning(tcIDs []string, toolResultIDs map[string]bool) bool {
	if len(tcIDs) == 0 {
		return false
	}
	for _, id := range tcIDs {
		if id == "" {
			continue
		}
		if !toolResultIDs[id] {
			return false
		}
	}
	return true
}

// injectReasoningContent 在转发请求前为 messages 中丢失 reasoning_content 的
// assistant 消息补回缓存值。注入策略（按优先级）：
//  1. 有 tool_calls → 用 tool_call_id 精确匹配（现有逻辑）
//  2. 无 tool_calls 且 content 非空 → 用 content 指纹匹配（新增，覆盖纯思考轮次）
//  3. 无 tool_calls 且 content 空 → Hermes L3 折叠场景，反查下一 tool 消息的 tool_call_id
//
// 安全约束：只在 tool_call_id 有对应 tool_result 时注入（避免消息链断裂）。
// 未命中跳过——尽力而为，不因缺缓存而失败。
func injectReasoningContent(body []byte, cache *ReasoningCache) ([]byte, bool) {
	if cache == nil {
		return body, false
	}
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return body, false
	}
	arr := msgs.Array()
	toolResultIDs := collectToolResultIDs(arr)

	nb := body
	changed := false
	for i := range arr {
		m := arr[i]
		if m.Get("role").String() != "assistant" {
			continue
		}
		if m.Get("reasoning_content").Exists() {
			continue
		}
		tcs := m.Get("tool_calls")

		// 策略 1: 有 tool_calls → 用 tool_call_id 匹配
		if tcs.IsArray() && len(tcs.Array()) > 0 {
			var tcIDs []string
			for _, tc := range tcs.Array() {
				if id := tc.Get("id").String(); id != "" {
					tcIDs = append(tcIDs, id)
				}
			}
			if len(tcIDs) == 0 {
				continue
			}
			if !shouldInjectReasoning(tcIDs, toolResultIDs) {
				continue
			}
			for _, id := range tcIDs {
				if id == "" {
					continue
				}
				// 命中即注入（含空串："该轮无思考"标记，注入空 reasoning_content
				// 可通过 DeepSeek thinking 模式回传校验）。
				if rc, ok := cache.Get(id); ok {
					var err error
					nb, err = sjson.SetBytes(nb, fmt.Sprintf("messages.%d.reasoning_content", i), rc)
					if err != nil {
						continue
					}
					changed = true
					break
				}
			}
			continue
		}

		// 策略 2: 无 tool_calls 且 content 非空 → 用指纹匹配（纯思考回答轮次）
		content := m.Get("content").String()
		if content != "" {
			if rc, ok := cache.GetByFingerprint(content); ok && rc != "" {
				var err error
				nb, err = sjson.SetBytes(nb, fmt.Sprintf("messages.%d.reasoning_content", i), rc)
				if err != nil {
					continue
				}
				changed = true
			}
			continue
		}

		// 策略 3: content 为空 → Hermes L3 折叠场景，反查下一 tool 消息的 tool_call_id。
		// 命中即注入（含空串标记）：与策略 1 一致，空 reasoning_content 也算已回传，
		// 可通过 DeepSeek thinking 模式校验。修复前要求 rc != ""，无思考轮次（跨上游
		// 混布 GLM 等）的空串标记被跳过，折叠场景仍会 400（2026-08-29 日志实测 bai 400）。
		for j := i + 1; j < len(arr); j++ {
			tm := arr[j]
			if tm.Get("role").String() == "tool" {
				if id := tm.Get("tool_call_id").String(); id != "" {
					if !toolResultIDs[id] {
						continue
					}
					if rc, ok := cache.Get(id); ok {
						var err error
						nb, err = sjson.SetBytes(nb, fmt.Sprintf("messages.%d.reasoning_content", i), rc)
						if err != nil {
							continue
						}
						changed = true
					}
				}
				break
			}
			break
		}
	}
	return nb, changed
}

// ===== 响应缓存逻辑 =====

// cacheReasoningFromMessage 从非流式响应体提取 assistant 消息的 reasoning_content：
//   - 有 tool_calls → 按 tool_call_id 缓存（供后续 tool-calling 请求回传）
//   - 无 tool_calls 但有 content → 按 content 指纹缓存（纯思考回答轮次）
//   - 两者都有 → 同时缓存两种 key（最大化命中率）
func cacheReasoningFromMessage(data []byte, cache *ReasoningCache) {
	if cache == nil {
		return
	}
	msg := gjson.GetBytes(data, "choices.0.message")
	if !msg.Exists() {
		return
	}
	rc := msg.Get("reasoning_content").String()
	tcs := msg.Get("tool_calls")
	content := msg.Get("content").String()
	// rc 为空且无 tool_calls：纯文本无思考轮次，无需缓存（回传校验只针对 tool_calls 轮次）
	if rc == "" && !(tcs.IsArray() && len(tcs.Array()) > 0) {
		return
	}

	// 有 tool_calls → 按 tool_call_id 缓存（rc 可为空串："该轮无思考"标记，
	// 下一轮注入空 reasoning_content 才能通过 DeepSeek thinking 回传校验）
	if tcs.IsArray() && len(tcs.Array()) > 0 {
		var ids []string
		for _, tc := range tcs.Array() {
			if id := tc.Get("id").String(); id != "" {
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 {
			cache.PutAll(ids, rc)
		}
	}

	// 有 content → 按指纹缓存（即使也有 tool_calls，双缓存最大化命中率）
	if content != "" && rc != "" {
		cache.PutFingerprint(content, rc)
	}
}

// cacheReasoningDelta 从流式 SSE 的单个 data chunk 中提取 reasoning_content 分片、
// content 分片与 tool_call id：累积拼接到对应 buf；tool_call id 出现的 chunk 到达时
// 把当前已累积的 reasoning 立即写入缓存（此时 reasoning 通常已完整）。
// 流结束（[DONE] / EOF）时由调用方用完整 buf 再写一次，覆盖 tool_calls 之后才输出的分片，
// 同时按 content 指纹缓存（纯思考回答轮次）。
func cacheReasoningDelta(data string, reasoningBuf, contentBuf *strings.Builder, ids *[]string, cache *ReasoningCache) {
	if cache == nil {
		return
	}
	delta := gjson.Get(data, "choices.0.delta")
	if !delta.Exists() {
		return
	}
	if rc := delta.Get("reasoning_content").String(); rc != "" {
		reasoningBuf.WriteString(rc)
	}
	if ct := delta.Get("content").String(); ct != "" {
		contentBuf.WriteString(ct)
	}
	tcs := delta.Get("tool_calls")
	if !tcs.IsArray() {
		return
	}
	for _, tc := range tcs.Array() {
		id := tc.Get("id").String()
		if id == "" {
			continue
		}
		*ids = append(*ids, id)
		// 无条件写入（含空串）：该轮模型可能未思考（GLM 等混布/ reasoning_effort=none）。
		// 空串也是有效标记——下一轮注入空 reasoning_content 才能通过 DeepSeek 官方
		// "thinking 模式必须回传"校验；只缓存非空会导致这类轮次注入失败 → 400。
		cache.Put(id, reasoningBuf.String())
	}
}
