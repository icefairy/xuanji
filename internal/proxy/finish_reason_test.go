package proxy

import (
	"strings"
	"testing"
)

// 本文件锁定「官方网关 finish_reason 归一化」这条约束。
//
// 背景（2026-10-21，对照 WorkBuddy 客户端逆向文档）：官方 SSE 的
// choices[0].finish_reason 会漏出 Anthropic 语义的 end_turn（内部
// StreamEventConverter 用 Anthropic 事件名），而 OpenAI 协议只有
// stop / length / tool_calls / content_filter。客户端按枚举判断时
// end_turn 既不是 stop 也不是 tool_calls，会被当成异常结束。

func TestNormalizeFinishReason(t *testing.T) {
	cases := map[string]string{
		"end_turn":        "stop",
		"tool_use":        "tool_calls",
		"stop":            "stop",
		"length":          "length",
		"tool_calls":      "tool_calls",
		"content_filter":  "content_filter",
		"":                "",
		"END_TURN":        "stop",
		"  end_turn  ":    "stop",
		"some_future_val": "some_future_val", // 未知值不猜，原样保留
	}
	for in, want := range cases {
		if got := normalizeFinishReason(in); got != want {
			t.Errorf("normalizeFinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeFinishReasonObject(t *testing.T) {
	body := []byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"end_turn"}]}`)
	out, changed := normalizeFinishReasonObject(body)
	if !changed {
		t.Fatal("end_turn 应被改写")
	}
	if !strings.Contains(string(out), `"finish_reason":"stop"`) {
		t.Errorf("改写后应为 stop: %s", string(out))
	}
	// 其余字段必须原样保留（不能重新序列化丢字段）
	if !strings.Contains(string(out), `"content":"ok"`) {
		t.Errorf("其它字段被破坏: %s", string(out))
	}
}

func TestNormalizeFinishReasonObjectNoop(t *testing.T) {
	body := []byte(`{"choices":[{"finish_reason":"stop"}]}`)
	out, changed := normalizeFinishReasonObject(body)
	if changed {
		t.Error("标准值不应改写")
	}
	if string(out) != string(body) {
		t.Error("无变化时应返回原 body")
	}
	if _, c := normalizeFinishReasonObject(nil); c {
		t.Error("nil body 不应改写")
	}
	if _, c := normalizeFinishReasonObject([]byte("not json")); c {
		t.Error("非法 JSON 不应改写")
	}
}

func TestNormalizeFinishReasonSSELine(t *testing.T) {
	line := `data: {"id":"x","choices":[{"delta":{},"finish_reason":"end_turn"}]}`
	out := string(normalizeFinishReasonSSELine([]byte(line)))
	if !strings.HasPrefix(out, "data: ") {
		t.Errorf("必须保留 data: 前缀: %q", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("end_turn 未归一化: %q", out)
	}

	// 标准值、[DONE]、非 data 行都必须原样返回（热路径零改写）
	for _, s := range []string{
		`data: {"choices":[{"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		`event: ping`,
		``,
	} {
		if got := string(normalizeFinishReasonSSELine([]byte(s))); got != s {
			t.Errorf("不应改写 %q，得到 %q", s, got)
		}
	}
}
