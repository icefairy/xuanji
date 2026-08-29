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
	c := NewReasoningCache(10, nil)
	c.Put("call_1", "deep thinking...")
	if v, ok := c.Get("call_1"); !ok || v != "deep thinking..." {
		t.Fatalf("Get(call_1) = %q, %v; want %q, true", v, ok, "deep thinking...")
	}
	if _, ok := c.Get("call_missing"); ok {
		t.Fatalf("Get(call_missing) should miss")
	}
	// 空 id 不缓存；空串内容现在也缓存（"该轮无思考"标记，
	// 注入空 reasoning_content 可通过 DeepSeek thinking 回传校验）
	c.Put("", "x")
	c.Put("call_empty", "")
	if v, ok := c.Get("call_empty"); !ok || v != "" {
		t.Fatalf("Get(call_empty) = %q, %v; want empty string hit", v, ok)
	}
	if c.Len() != 2 {
		t.Fatalf("Len = %d, want 2 (only empty id ignored)", c.Len())
	}
}

func TestReasoningCache_PutAll(t *testing.T) {
	c := NewReasoningCache(10, nil)
	c.PutAll([]string{"a", "b", "c"}, "shared reasoning")
	for _, id := range []string{"a", "b", "c"} {
		if v, ok := c.Get(id); !ok || v != "shared reasoning" {
			t.Fatalf("Get(%s) = %q, %v; want shared reasoning", id, v, ok)
		}
	}
}

func TestReasoningCache_FIFOEvict(t *testing.T) {
	c := NewReasoningCache(2, nil)
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
	c := NewReasoningCache(2, nil)
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
	c := NewReasoningCache(10, nil)
	c.Put("call_abc", "thinking about the weather")
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_abc","content":"sunny"}]}`)
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
	c := NewReasoningCache(10, nil)
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
	c := NewReasoningCache(10, nil)
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
	c := NewReasoningCache(10, nil)
	c.Put("call_2", "multi reasoning")
	// 两个 tool_call 均已执行完（有对应 tool_result），第二个命中 → 注入同一份 reasoning
	body := []byte(`{"model":"m","messages":[
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f1","arguments":"{}"}},{"id":"call_2","type":"function","function":{"name":"f2","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"r1"},
		{"role":"tool","tool_call_id":"call_2","content":"r2"}
	]}`)
	nb, changed := injectReasoningContent(body, c)
	if !changed {
		t.Fatalf("should inject when tool_calls have matching tool_results and any id hits")
	}
	if got := gjson.GetBytes(nb, "messages.0.reasoning_content").String(); got != "multi reasoning" {
		t.Fatalf("reasoning_content = %q, want multi reasoning", got)
	}
	// 两个 tool_call 保持原样
	if got := gjson.GetBytes(nb, "messages.0.tool_calls.#").Int(); got != 2 {
		t.Fatalf("tool_calls count = %d, want 2", got)
	}
}

func TestInjectReasoningContent_NewToolCall_NoInject(t *testing.T) {
	c := NewReasoningCache(10, nil)
	c.Put("call_new", "reasoning for new call")
	// 新发起的 tool_call（无对应 tool_result）：注入 reasoning_content 会让上游认为
	// 这是历史 context 导致消息链断裂（tool_call 找不到 tool_result）→ 400。必须跳过。
	body := []byte(`{"model":"m","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"call_new","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`)
	if nb, changed := injectReasoningContent(body, c); changed || string(nb) != string(body) {
		t.Fatalf("new tool_call without tool_result must not inject")
	}
}

func TestInjectReasoningContent_PartialResult_NoInject(t *testing.T) {
	c := NewReasoningCache(10, nil)
	c.Put("call_2", "reasoning")
	// 两个 tool_call，只有 call_1 有 tool_result（call_2 是新发起）→ 保守不注入
	body := []byte(`{"model":"m","messages":[
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f1","arguments":"{}"}},{"id":"call_2","type":"function","function":{"name":"f2","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"r1"}
	]}`)
	if nb, changed := injectReasoningContent(body, c); changed || string(nb) != string(body) {
		t.Fatalf("partially-resolved tool_calls must not inject (conservative)")
	}
}

func TestInjectReasoningContent_NoToolCalls(t *testing.T) {
	c := NewReasoningCache(10, nil)
	c.Put("call_1", "x")
	// 普通 assistant 消息（无 tool_calls，无匹配指纹）不注入
	body := []byte(`{"model":"m","messages":[{"role":"assistant","content":"plain answer"}]}`)
	if nb, changed := injectReasoningContent(body, c); changed || string(nb) != string(body) {
		t.Fatalf("plain assistant message without matching fingerprint must not change")
	}
	// 非 assistant 消息不注入
	body = []byte(`{"model":"m","messages":[{"role":"tool","tool_call_id":"call_1","content":"result"}]}`)
	if nb, changed := injectReasoningContent(body, c); changed || string(nb) != string(body) {
		t.Fatalf("tool message must not change")
	}
}

func TestInjectReasoningContent_Fingerprint(t *testing.T) {
	c := NewReasoningCache(10, nil)
	c.PutFingerprint("thinking result", "deep reasoning here")
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"thinking result"}]}`)
	nb, changed := injectReasoningContent(body, c)
	if !changed {
		t.Fatalf("should inject reasoning_content via fingerprint")
	}
	msg := gjson.GetBytes(nb, "messages.1")
	if got := msg.Get("reasoning_content").String(); got != "deep reasoning here" {
		t.Fatalf("reasoning_content = %q, want deep reasoning here", got)
	}
	// 其他字段零破坏
	if got := msg.Get("content").String(); got != "thinking result" {
		t.Fatalf("content = %q, want thinking result", got)
	}
	if got := msg.Get("role").String(); got != "assistant" {
		t.Fatalf("role = %q, want assistant", got)
	}
}

// ===== 响应解析测试 =====

func TestCacheReasoningFromMessage_NonStream(t *testing.T) {
	c := NewReasoningCache(10, nil)
	resp := []byte(`{"id":"r","choices":[{"index":0,"message":{"role":"assistant","content":null,"reasoning_content":"non-stream reasoning","tool_calls":[{"id":"call_ns","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	cacheReasoningFromMessage(resp, c)
	if v, ok := c.Get("call_ns"); !ok || v != "non-stream reasoning" {
		t.Fatalf("Get(call_ns) = %q, %v; want non-stream reasoning", v, ok)
	}
}

func TestCacheReasoningFromMessage_NoToolCalls(t *testing.T) {
	// 无 tool_calls 但有 content 和 reasoning_content → 按指纹缓存
	c := NewReasoningCache(10, nil)
	resp := []byte(`{"choices":[{"message":{"role":"assistant","content":"ok","reasoning_content":"thought here"}}]}`)
	cacheReasoningFromMessage(resp, c)
	// 不应有 tool_call 缓存（无 tool_calls）
	if c.Len() != 0 {
		t.Fatalf("Len = %d, want 0 (no tool_call id to key on)", c.Len())
	}
	// 但应有指纹缓存
	if v, ok := c.GetByFingerprint("ok"); !ok || v != "thought here" {
		t.Fatalf("GetByFingerprint(ok) = %q, %v; want thought here", v, ok)
	}
}

func TestCacheReasoningFromMessage_EmptyReasoning(t *testing.T) {
	// 空 reasoning_content + tool_calls：现在也缓存空串（"该轮无思考"标记，
	// 下一轮注入空 reasoning_content 才能通过 DeepSeek thinking 回传校验）
	c := NewReasoningCache(10, nil)
	resp := []byte(`{"choices":[{"message":{"role":"assistant","content":null,"reasoning_content":"","tool_calls":[{"id":"call_x","type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`)
	cacheReasoningFromMessage(resp, c)
	if v, ok := c.Get("call_x"); !ok || v != "" {
		t.Fatalf("Get(call_x) = %q, %v; want empty string hit", v, ok)
	}
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1 (empty marker cached for tool_calls)", c.Len())
	}
}

func TestCacheReasoningDelta_StreamConcat(t *testing.T) {
	c := NewReasoningCache(10, nil)
	var reasoningBuf strings.Builder
	var contentBuf strings.Builder
	var ids []string
	// 分片累积：thinking 模式流式 reasoning_content 是逐片 delta
	cacheReasoningDelta(`{"choices":[{"delta":{"role":"assistant","reasoning_content":"step1 "},"finish_reason":null}]}`, &reasoningBuf, &contentBuf, &ids, c)
	cacheReasoningDelta(`{"choices":[{"delta":{"reasoning_content":"step2 "},"finish_reason":null}]}`, &reasoningBuf, &contentBuf, &ids, c)
	cacheReasoningDelta(`{"choices":[{"delta":{"reasoning_content":"step3"},"finish_reason":null}]}`, &reasoningBuf, &contentBuf, &ids, c)
	// tool_call id 出现的 chunk：立即写入当前累积
	cacheReasoningDelta(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_s","type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":null}]}`, &reasoningBuf, &contentBuf, &ids, c)
	if len(ids) != 1 || ids[0] != "call_s" {
		t.Fatalf("ids = %v, want [call_s]", ids)
	}
	if got := reasoningBuf.String(); got != "step1 step2 step3" {
		t.Fatalf("reasoningBuf = %q, want step1 step2 step3", got)
	}
	if v, ok := c.Get("call_s"); !ok || v != "step1 step2 step3" {
		t.Fatalf("Get(call_s) = %q, %v; want full concatenated reasoning", v, ok)
	}
	// arguments 增量 chunk 不带 id → 不重复记录
	cacheReasoningDelta(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"beijing\"}"}}]},"finish_reason":null}]}`, &reasoningBuf, &contentBuf, &ids, c)
	if len(ids) != 1 {
		t.Fatalf("ids must not grow on argument-only chunks, got %v", ids)
	}
	// 流结束补写（模拟调用方在 [DONE]/EOF 时 PutAll 完整 buf + fingerprint）
	c.PutAll(ids, reasoningBuf.String())
	if v, _ := c.Get("call_s"); v != "step1 step2 step3" {
		t.Fatalf("final reasoning = %q, want step1 step2 step3", v)
	}
	// 带 content 的分片验证指纹缓存
	var cb2 strings.Builder
	cacheReasoningDelta(`{"choices":[{"delta":{"content":"Hello "},"finish_reason":null}]}`, &reasoningBuf, &cb2, &ids, c)
	cacheReasoningDelta(`{"choices":[{"delta":{"content":"World"},"finish_reason":null}]}`, &reasoningBuf, &cb2, &ids, c)
	c.PutFingerprint(cb2.String(), reasoningBuf.String())
	if v, ok := c.GetByFingerprint("Hello World"); !ok || v != "step1 step2 step3" {
		t.Fatalf("GetByFingerprint(Hello World) = %q, %v; want step1 step2 step3", v, ok)
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

// reasoningEmptyUpstreamBody 是无思考轮次的 tool-calling 响应：模型未产生 reasoning_content
// （跨上游混布场景，如 GLM 等），但仍有 tool_calls。网关必须缓存空串标记。
const reasoningEmptyUpstreamBody = `{"id":"r2","object":"chat.completion","model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_nothink","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`

// strictReasoningUpstream 模拟 DeepSeek 官方 thinking 模式校验（bai 等中转实测行为）：
// assistant 消息若带 tool_calls，必须携带 reasoning_content 字段（空串也算已回传），
// 否则返回 400 "The reasoning_content in the thinking mode must be passed back to the API."。
// firstBody 是第 1 次请求（仅 user 消息）的响应体，后续请求（带历史 tool_calls）校验通过后
// 返回 finalBody。
func strictReasoningUpstream(gotBodies *[][]byte, firstBody, finalBody string) http.HandlerFunc {
	var n int
	return func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		*gotBodies = append(*gotBodies, data)
		// 遍历所有 assistant 消息：带 tool_calls 的必须已有 reasoning_content 字段
		for _, m := range gjson.GetBytes(data, "messages").Array() {
			if m.Get("role").String() != "assistant" {
				continue
			}
			if !m.Get("tool_calls").IsArray() || len(m.Get("tool_calls").Array()) == 0 {
				continue
			}
			if !m.Get("reasoning_content").Exists() {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				// 注意：错误文案中的反引号不能用 raw string（会终止字符串），改用拼接
				msg := "The `reasoning_content` in the thinking mode must be passed back to the API."
				fmt.Fprintf(w, `{"error":{"message":%q,"type":"invalid_request_error","code":"invalid_request_error"}}`, msg)
				return
			}
		}
		n++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if n <= 1 {
			fmt.Fprint(w, firstBody)
		} else {
			fmt.Fprint(w, finalBody)
		}
	}
}

// TestChatCompletions_ReasoningPassthrough_StrictUpstream 验证核心回填链路：
// 客户端在第二轮丢掉了 reasoning_content（多轮 tool-calling 里客户端
// 规范化/重建历史消息时常见），严格上游（DeepSeek 官方/bai 风格）会直接 400。
// 璇玑网关心须从缓存回填 reasoning_content，使上游校验通过、请求正常返回 200。
func TestChatCompletions_ReasoningPassthrough_StrictUpstream(t *testing.T) {
	var gotBodies [][]byte
	upstream, h := newTestHandler(t, strictReasoningUpstream(&gotBodies, reasoningUpstreamBody, `{"id":"fin","object":"chat.completion","model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"天气晴"},"finish_reason":"stop"}]}`))
	defer upstream.Close()
	h.cfg.Proxy.CacheReasoningContent = true

	// 第一轮：仅 user 消息 → 上游返回 tool_calls + reasoning_content，网关缓存
	if rec := doChat(t, h, `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"查询北京天气"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("first round status = %d", rec.Code)
	}
	if v, ok := h.reasoning.Get("call_e2e"); !ok || v != "thinking about tools" {
		t.Fatalf("cache after first round = %q, %v; want thinking about tools", v, ok)
	}

	// 第二轮：客户端丢掉了 assistant 的 reasoning_content（只保留 tool_calls）
	secondBody := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"北京天气怎么样"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_e2e","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_e2e","content":"晴"}]}`
	rec := doChat(t, h, secondBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("second round status = %d, want 200 (gateway must backfill reasoning_content to avoid 400)\nbody: %s", rec.Code, rec.Body.String())
	}
	if len(gotBodies) < 2 {
		t.Fatalf("upstream called %d times, want >= 2", len(gotBodies))
	}
	// 上游实际收到的第二轮请求必须包含回填的 reasoning_content
	if got := gjson.GetBytes(gotBodies[1], "messages.1.reasoning_content").String(); got != "thinking about tools" {
		t.Fatalf("upstream received reasoning_content = %q, want thinking about tools (backfill failed)", got)
	}
}

// TestChatCompletions_ReasoningPassthrough_NoInjection_Strict400 反向对照：
// 缓存开关关闭时网关不做回填，严格上游应返回 400 —— 证明 StrictUpstream 的
// 校验确实拦截缺字段请求，即主测试的 200 是回填生效的结果而非 mock 放行。
func TestChatCompletions_ReasoningPassthrough_NoInjection_Strict400(t *testing.T) {
	var gotBodies [][]byte
	upstream, h := newTestHandler(t, strictReasoningUpstream(&gotBodies, reasoningUpstreamBody, `{"id":"fin","object":"chat.completion","model":"deepseek-v4-flash","choices":[]}`))
	defer upstream.Close()
	h.cfg.Proxy.CacheReasoningContent = false // 关闭回填 → 应 400

	// 第一轮（user-only，校验通过）
	if rec := doChat(t, h, `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"查询北京天气"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("first round status = %d", rec.Code)
	}

	// 第二轮：缺 reasoning_content，网关不回填 → 严格上游 400
	secondBody := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"北京天气怎么样"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_e2e","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_e2e","content":"晴"}]}`
	rec := doChat(t, h, secondBody)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("second round status = %d, want 400 (strict upstream must reject missing reasoning_content when backfill disabled)", rec.Code)
	}
}

// TestChatCompletions_ReasoningPassthrough_NoThinkingRound 验证无思考轮次场景：
// 第一轮模型未产生 reasoning_content（跨上游混布，GLM 等不思考时），tool_calls 仍存在。
// 网关须缓存空串标记；第二轮客户端丢字段后回填空串 reasoning_content（空串也算已回传），
// 严格上游校验通过，不报 400。
func TestChatCompletions_ReasoningPassthrough_NoThinkingRound(t *testing.T) {
	var gotBodies [][]byte
	upstream, h := newTestHandler(t, strictReasoningUpstream(&gotBodies, reasoningEmptyUpstreamBody, `{"id":"fin","object":"chat.completion","model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"42"},"finish_reason":"stop"}]}`))
	defer upstream.Close()
	h.cfg.Proxy.CacheReasoningContent = true

	// 第一轮：上游返回 tool_calls 但无 reasoning_content → 网关缓存空串标记
	if rec := doChat(t, h, `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"算一下"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("first round status = %d", rec.Code)
	}
	if v, ok := h.reasoning.Get("call_nothink"); !ok {
		t.Fatalf("cache after no-thinking round: tool_call_id missing (must cache empty marker)")
	} else if v != "" {
		t.Fatalf("cache value = %q, want empty string (no-thinking round)", v)
	}

	// 第二轮：客户端丢 reasoning_content → 网关回填空串 → 严格上游校验通过（空串也算已回传）
	secondBody := `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"算一下"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_nothink","type":"function","function":{"name":"calc","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_nothink","content":"42"}]}`
	rec := doChat(t, h, secondBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("second round status = %d, want 200 (empty reasoning_content backfill must pass strict upstream)\nbody: %s", rec.Code, rec.Body.String())
	}
	if len(gotBodies) < 2 {
		t.Fatalf("upstream called %d times, want >= 2", len(gotBodies))
	}
	// 上游收到的消息里 reasoning_content 字段必须存在（值为空串也满足回传语义）
	if !gjson.GetBytes(gotBodies[1], "messages.1.reasoning_content").Exists() {
		t.Fatalf("upstream received message without reasoning_content field (backfill failed)")
	}
}
