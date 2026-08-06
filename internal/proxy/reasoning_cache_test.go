package proxy

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// ===== ReasoningCache 单元测试 =====

func TestReasoningCache_PutGet(t *testing.T) {
	c := NewReasoningCache(10)
	c.Put("call_1", "deep thinking...")
	if v, ok := c.Get("call_1"); !ok || v != "deep thinking..." {
		t.Fatalf("Get(call_1) = %q, %v; want %q, true", v, ok, "deep thinking...")
	}
	if _, ok := c.Get("call_missing"); ok {
		t.Fatalf("Get(call_missing) should miss")
	}
	// 空 id / 空内容不缓存
	c.Put("", "x")
	c.Put("call_empty", "")
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1 (empty entries ignored)", c.Len())
	}
}

func TestReasoningCache_PutAll(t *testing.T) {
	c := NewReasoningCache(10)
	c.PutAll([]string{"a", "b", "c"}, "shared reasoning")
	for _, id := range []string{"a", "b", "c"} {
		if v, ok := c.Get(id); !ok || v != "shared reasoning" {
			t.Fatalf("Get(%s) = %q, %v; want shared reasoning", id, v, ok)
		}
	}
}

func TestReasoningCache_FIFOEvict(t *testing.T) {
	c := NewReasoningCache(2)
	c.Put("a", "1")
	c.Put("b", "2")
	c.Put("c", "3") // 超出容量，淘汰最旧 a
	if c.Len() != 2 {
		t.Fatalf("Len = %d, want 2", c.Len())
	}
	if _, ok := c.Get("a"); ok {
		t.Fatalf("oldest entry a must be evicted")
	}
	if v, _ := c.Get("b"); v != "2" {
		t.Fatalf("b = %q, want 2", v)
	}
	if v, _ := c.Get("c"); v != "3" {
		t.Fatalf("c = %q, want 3", v)
	}
}

func TestReasoningCache_UpdateKeepsOrder(t *testing.T) {
	// 更新已存在的 key 不应改变插入顺序（避免热 key 把冷 key 挤出去）
	c := NewReasoningCache(2)
	c.Put("a", "1")
	c.Put("b", "2")
	c.Put("a", "updated") // 更新 a，不改变顺序
	c.Put("c", "3")       // 应淘汰最旧的 a（顺序未变）
	if _, ok := c.Get("a"); ok {
		t.Fatalf("a must be evicted (order preserved on update)")
	}
	if v, _ := c.Get("b"); v != "2" {
		t.Fatalf("b = %q, want 2", v)
	}
	if v, _ := c.Get("c"); v != "3" {
		t.Fatalf("c = %q, want 3", v)
	}
}

// ===== 注入逻辑测试 =====

func TestInjectReasoningContent_Hit(t *testing.T) {
	c := NewReasoningCache(10)
	c.Put("call_abc", "thinking about the weather")
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":"{}"}}]}]}`)
	nb, changed := injectReasoningContent(body, c)
	if !changed {
		t.Fatalf("should inject reasoning_content")
	}
	msg := gjson.GetBytes(nb, "messages.1")
	if got := msg.Get("reasoning_content").String(); got != "thinking about the weather" {
		t.Fatalf("reasoning_content = %q, want thinking about the weather", got)
	}
	// 其他字段零破坏
	if got := msg.Get("role").String(); got != "assistant" {
		t.Fatalf("role = %q, want assistant", got)
	}
	if got := msg.Get("tool_calls.0.id").String(); got != "call_abc" {
		t.Fatalf("tool_calls.0.id = %q, want call_abc", got)
	}
	if got := msg.Get("tool_calls.0.function.name").String(); got != "get_weather" {
		t.Fatalf("tool_calls.0.function.name = %q, want get_weather", got)
	}
	if got := gjson.GetBytes(nb, "messages.0.content").String(); got != "hi" {
		t.Fatalf("messages.0.content = %q, want hi", got)
	}
}

func TestInjectReasoningContent_Miss(t *testing.T) {
	c := NewReasoningCache(10)
	body := []byte(`{"model":"m","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"call_unknown","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`)
	nb, changed := injectReasoningContent(body, c)
	if changed {
		t.Fatalf("unmatched tool_call_id should not change body")
	}
	if string(nb) != string(body) {
		t.Fatalf("body must be unchanged: %s", nb)
	}
}

func TestInjectReasoningContent_AlreadyHas(t *testing.T) {
	c := NewReasoningCache(10)
	c.Put("call_1", "cached reasoning")
	body := []byte(`{"model":"m","messages":[{"role":"assistant","reasoning_content":"client keeps it","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`)
	nb, changed := injectReasoningContent(body, c)
	if changed {
		t.Fatalf("message already has reasoning_content, must not be touched")
	}
	if got := gjson.GetBytes(nb, "messages.0.reasoning_content").String(); got != "client keeps it" {
		t.Fatalf("reasoning_content = %q, want client keeps it", got)
	}
}

func TestInjectReasoningContent_MultiToolCall(t *testing.T) {
	c := NewReasoningCache(10)
	c.Put("call_2", "multi reasoning")
	// 两个 tool_call，第二个命中 → 注入同一份 reasoning
	body := []byte(`{"model":"m","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f1","arguments":"{}"}},{"id":"call_2","type":"function","function":{"name":"f2","arguments":"{}"}}]}]}`)
	nb, changed := injectReasoningContent(body, c)
	if !changed {
		t.Fatalf("should inject when any tool_call_id hits")
	}
	if got := gjson.GetBytes(nb, "messages.0.reasoning_content").String(); got != "multi reasoning" {
		t.Fatalf("reasoning_content = %q, want multi reasoning", got)
	}
	// 两个 tool_call 保持原样
	if got := gjson.GetBytes(nb, "messages.0.tool_calls.#").Int(); got != 2 {
		t.Fatalf("tool_calls count = %d, want 2", got)
	}
}

func TestInjectReasoningContent_NoToolCalls(t *testing.T) {
	c := NewReasoningCache(10)
	c.Put("call_1", "x")
	// 普通 assistant 消息（无 tool_calls）不注入
	body := []byte(`{"model":"m","messages":[{"role":"assistant","content":"plain answer"}]}`)
	if nb, changed := injectReasoningContent(body, c); changed || string(nb) != string(body) {
		t.Fatalf("plain assistant message must not change")
	}
	// 非 assistant 消息不注入
	body = []byte(`{"model":"m","messages":[{"role":"tool","tool_call_id":"call_1","content":"result"}]}`)
	if nb, changed := injectReasoningContent(body, c); changed || string(nb) != string(body) {
		t.Fatalf("tool message must not change")
	}
}

// ===== 响应解析测试 =====

func TestCacheReasoningFromMessage_NonStream(t *testing.T) {
	c := NewReasoningCache(10)
	resp := []byte(`{"id":"r","choices":[{"index":0,"message":{"role":"assistant","content":null,"reasoning_content":"non-stream reasoning","tool_calls":[{"id":"call_ns","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	cacheReasoningFromMessage(resp, c)
	if v, ok := c.Get("call_ns"); !ok || v != "non-stream reasoning" {
		t.Fatalf("Get(call_ns) = %q, %v; want non-stream reasoning", v, ok)
	}
}

func TestCacheReasoningFromMessage_NoToolCalls(t *testing.T) {
	// 无 tool_calls（普通回答）不缓存
	c := NewReasoningCache(10)
	resp := []byte(`{"choices":[{"message":{"role":"assistant","content":"ok","reasoning_content":"thought here"}}]}`)
	cacheReasoningFromMessage(resp, c)
	if c.Len() != 0 {
		t.Fatalf("Len = %d, want 0 (no tool_call id to key on)", c.Len())
	}
}

func TestCacheReasoningFromMessage_EmptyReasoning(t *testing.T) {
	// 空 reasoning_content 不缓存
	c := NewReasoningCache(10)
	resp := []byte(`{"choices":[{"message":{"role":"assistant","content":null,"reasoning_content":"","tool_calls":[{"id":"call_x","type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`)
	cacheReasoningFromMessage(resp, c)
	if c.Len() != 0 {
		t.Fatalf("Len = %d, want 0 (empty reasoning ignored)", c.Len())
	}
}

func TestCacheReasoningDelta_StreamConcat(t *testing.T) {
	c := NewReasoningCache(10)
	var buf strings.Builder
	var ids []string
	// 分片累积：thinking 模式流式 reasoning_content 是逐片 delta
	cacheReasoningDelta(`{"choices":[{"delta":{"role":"assistant","reasoning_content":"step1 "},"finish_reason":null}]}`, &buf, &ids, c)
	cacheReasoningDelta(`{"choices":[{"delta":{"reasoning_content":"step2 "},"finish_reason":null}]}`, &buf, &ids, c)
	cacheReasoningDelta(`{"choices":[{"delta":{"reasoning_content":"step3"},"finish_reason":null}]}`, &buf, &ids, c)
	// tool_call id 出现的 chunk：立即写入当前累积
	cacheReasoningDelta(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_s","type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":null}]}`, &buf, &ids, c)
	if len(ids) != 1 || ids[0] != "call_s" {
		t.Fatalf("ids = %v, want [call_s]", ids)
	}
	if got := buf.String(); got != "step1 step2 step3" {
		t.Fatalf("buf = %q, want step1 step2 step3", got)
	}
	if v, ok := c.Get("call_s"); !ok || v != "step1 step2 step3" {
		t.Fatalf("Get(call_s) = %q, %v; want full concatenated reasoning", v, ok)
	}
	// arguments 增量 chunk 不带 id → 不重复记录
	cacheReasoningDelta(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"beijing\"}"}}]},"finish_reason":null}]}`, &buf, &ids, c)
	if len(ids) != 1 {
		t.Fatalf("ids must not grow on argument-only chunks, got %v", ids)
	}
	// 流结束补写（模拟调用方在 [DONE]/EOF 时 PutAll 完整 buf）
	c.PutAll(ids, buf.String())
	if v, _ := c.Get("call_s"); v != "step1 step2 step3" {
		t.Fatalf("final reasoning = %q, want step1 step2 step3", v)
	}
}

// ===== 端到端测试（Handler 集成）=====

// reasoningUpstreamBody 是 DeepSeek thinking 模式 tool-calling 的非流式响应。
const reasoningUpstreamBody = `{"id":"r1","object":"chat.completion","model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":null,"reasoning_content":"thinking about tools","tool_calls":[{"id":"call_e2e","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"beijing\"}"}}]},"finish_reason":"tool_calls"}]}`

// TestChatCompletions_ReasoningCache_NonStreamE2E 验证：非流式响应缓存 reasoning_content，
// 下一轮带历史 tool_calls 的请求自动补回（上游收到 reasoning_content，不再 400）。
func TestChatCompletions_ReasoningCache_NonStreamE2E(t *testing.T) {
	var gotBodies [][]byte
	upstream, h := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		gotBodies = append(gotBodies, data)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, reasoningUpstreamBody)
	})
	defer upstream.Close()
	h.cfg.Proxy.CacheReasoningContent = true

	// 第一轮：触发上游响应，网关缓存 reasoning_content
	rec := doChat(t, h, `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"查询北京天气"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("first round status = %d", rec.Code)
	}
	if v, ok := h.reasoning.Get("call_e2e"); !ok || v != "thinking about tools" {
		t.Fatalf("cache after first round = %q, %v; want thinking about tools", v, ok)
	}

	// 第二轮：客户端规范化丢掉了 reasoning_content（带历史 tool_calls）
	secondBody := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"北京天气怎么样"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_e2e","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"beijing\"}"}}]},{"role":"tool","tool_call_id":"call_e2e","content":"{\"weather\":\"晴\"}"}]}`
	rec = doChat(t, h, secondBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("second round status = %d", rec.Code)
	}
	if len(gotBodies) < 2 {
		t.Fatalf("upstream called %d times, want >= 2", len(gotBodies))
	}
	upstreamBody := gotBodies[1]
	if got := gjson.GetBytes(upstreamBody, "messages.1.reasoning_content").String(); got != "thinking about tools" {
		t.Fatalf("upstream received reasoning_content = %q, want thinking about tools", got)
	}
	// 其余字段不受影响
	if got := gjson.GetBytes(upstreamBody, "messages.1.tool_calls.0.id").String(); got != "call_e2e" {
		t.Fatalf("tool_calls.0.id = %q, want call_e2e", got)
	}
	if got := gjson.GetBytes(upstreamBody, "messages.2.role").String(); got != "tool" {
		t.Fatalf("messages.2.role = %q, want tool", got)
	}
}

// TestChatCompletions_ReasoningCache_StreamE2E 验证：流式响应 delta 分片拼接后缓存，
// 下一轮请求自动补回。
func TestChatCompletions_ReasoningCache_StreamE2E(t *testing.T) {
	const sse = "data: {\"id\":\"s1\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"thinking \"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{\"reasoning_content\":\"step by step\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_stream\",\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"\"}}]},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	var gotBodies [][]byte
	upstream, h := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		gotBodies = append(gotBodies, data)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, sse)
	})
	defer upstream.Close()
	h.cfg.Proxy.CacheReasoningContent = true

	// 第一轮：流式响应 → 缓存完整拼接的 reasoning
	rec := doChat(t, h, `{"model":"deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("stream status = %d", rec.Code)
	}
	if v, ok := h.reasoning.Get("call_stream"); !ok || v != "thinking step by step" {
		t.Fatalf("cache after stream = %q, %v; want %q", v, ok, "thinking step by step")
	}

	// 第二轮：丢 reasoning_content 的 tool 历史 → 自动补回
	secondBody := `{"model":"deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_stream","type":"function","function":{"name":"f","arguments":""}}]},{"role":"tool","tool_call_id":"call_stream","content":"42"}]}`
	rec = doChat(t, h, secondBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("second stream status = %d", rec.Code)
	}
	if len(gotBodies) < 2 {
		t.Fatalf("upstream called %d times, want >= 2", len(gotBodies))
	}
	if got := gjson.GetBytes(gotBodies[1], "messages.1.reasoning_content").String(); got != "thinking step by step" {
		t.Fatalf("upstream received reasoning_content = %q, want %q", got, "thinking step by step")
	}
}

// TestChatCompletions_ReasoningCache_Disabled 验证：开关关闭时缓存不写、注入不执行，
// body 原样透传（上游收到的 messages 与客户端发送完全一致）。
func TestChatCompletions_ReasoningCache_Disabled(t *testing.T) {
	var gotBodies [][]byte
	upstream, h := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		gotBodies = append(gotBodies, data)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, reasoningUpstreamBody)
	})
	defer upstream.Close()
	h.cfg.Proxy.CacheReasoningContent = false // 开关关闭（默认行为）

	// 第一轮：响应含 reasoning+tool_calls → 缓存不写
	rec := doChat(t, h, `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if h.reasoning.Len() != 0 {
		t.Fatalf("cache Len = %d, want 0 (switch off, no write)", h.reasoning.Len())
	}

	// 第二轮：带历史 tool_calls → 注入不执行，body 原样透传
	secondBody := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_e2e","type":"function","function":{"name":"get_weather","arguments":"{}"}}]}]}`
	rec = doChat(t, h, secondBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("second status = %d", rec.Code)
	}
	if len(gotBodies) < 2 {
		t.Fatalf("upstream called %d times, want >= 2", len(gotBodies))
	}
	upstreamMsgs := gjson.GetBytes(gotBodies[1], "messages").Raw
	clientMsgs := gjson.Get(secondBody, "messages").Raw
	if upstreamMsgs != clientMsgs {
		t.Fatalf("messages not passthrough when switch off:\n upstream=%s\n client  =%s", upstreamMsgs, clientMsgs)
	}
	if got := gjson.GetBytes(gotBodies[1], "messages.1.reasoning_content").Exists(); got {
		t.Fatalf("reasoning_content must NOT be injected when switch off")
	}
}
