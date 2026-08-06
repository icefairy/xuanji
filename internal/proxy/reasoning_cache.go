// Package proxy reasoning_content 回传兼容：DeepSeek V4 系列（deepseek-v4-flash/pro）
// 默认开启 thinking 模式，多轮 Tool-Calling 时要求 assistant 消息的 reasoning_content
// 在后续请求中原样回传，否则上游返回 400。客户端 agent（pi / Claude Code / Hermes 等）
// 在消息规范化时可能丢掉该字段，网关在此做兼容层：
//   - 转发响应时解析 assistant 消息/delta 中的 reasoning_content，按 tool_call_id 缓存
//   - 转发请求前为丢失该字段的 assistant 消息自动补回（系统设置开关，默认开启）
package proxy

import (
	"fmt"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// defaultReasoningCacheMax 是 reasoning_content 缓存的最大条目数，超出后 FIFO 淘汰最旧。
const defaultReasoningCacheMax = 500

// ReasoningCache 是 reasoning_content 的内存缓存，key=tool_call_id。
// 缓存 key 用 tool_call_id 而非会话 id：无需追踪会话状态，天然适配多客户端
// （n8n / Goose / Cursor / JetBrains 等 agent 框架各自携带历史 tool_calls）。
// 容量上限 defaultReasoningCacheMax，超出淘汰最旧（FIFO）。
// 尽力而为：命中就注入，未命中不影响正常转发。
type ReasoningCache struct {
	mu    sync.Mutex
	max   int
	items map[string]string // tool_call_id → reasoning_content
	order []string          // 插入顺序（FIFO 淘汰）
}

// NewReasoningCache 创建 reasoning_content 缓存。max<=0 时用默认值 500。
func NewReasoningCache(max int) *ReasoningCache {
	if max <= 0 {
		max = defaultReasoningCacheMax
	}
	return &ReasoningCache{max: max, items: make(map[string]string)}
}

// Put 写入 tool_call_id 对应的 reasoning_content。已存在时更新值但保持原插入顺序
// （避免"热 key"把其他条目挤出）；空 id 或空内容不缓存（注入空串无意义）。
func (c *ReasoningCache) Put(id, content string) {
	if id == "" || content == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.items[id]; exists {
		c.items[id] = content
		return
	}
	c.items[id] = content
	c.order = append(c.order, id)
	for len(c.order) > c.max {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.items, oldest)
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
func (c *ReasoningCache) Get(id string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.items[id]
	return v, ok
}

// Len 返回当前缓存条目数。
func (c *ReasoningCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// injectReasoningContent 在转发请求前为 messages 中丢失 reasoning_content 的
// assistant 消息补回缓存值：找到 role=assistant 且含 tool_calls 但无
// reasoning_content 字段的消息，用 tool_calls[].id 查缓存，命中则注入该字段。
// 只补 reasoning_content，不碰 role/content/tool_calls 等字段（零破坏）；
// 未命中跳过——尽力而为，不因缺缓存而失败。返回修改后的 body 与是否发生修改。
func injectReasoningContent(body []byte, cache *ReasoningCache) ([]byte, bool) {
	if cache == nil {
		return body, false
	}
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return body, false
	}
	arr := msgs.Array()
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
		if !tcs.IsArray() || len(tcs.Array()) == 0 {
			continue
		}
		// 多 tool_call 时任一 id 命中即注入同一份 reasoning（共享思考内容）
		for _, tc := range tcs.Array() {
			id := tc.Get("id").String()
			if id == "" {
				continue
			}
			if rc, ok := cache.Get(id); ok && rc != "" {
				var err error
				nb, err = sjson.SetBytes(nb, fmt.Sprintf("messages.%d.reasoning_content", i), rc)
				if err != nil {
					continue // 单条失败不影响其余消息
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
