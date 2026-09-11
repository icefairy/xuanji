package proxy

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestMergeSystemMessages(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantChg   bool
		wantSys   int    // 合并后 messages 中 system 数量
		wantFirst bool   // system 是否在第一位
		wantText  string // 合并后 system content
	}{
		{
			name:      "多条system字符串拼接",
			body:      `{"model":"m1","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"hi"},{"role":"system","content":"你是道家学者"}]}`,
			wantChg:   true,
			wantSys:   1,
			wantFirst: true,
			wantText:  "你是助手\n\n你是道家学者",
		},
		{
			name:      "单条system不变",
			body:      `{"model":"m1","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"hi"}]}`,
			wantChg:   false,
			wantSys:   1,
			wantFirst: true,
		},
		{
			name:    "无system不变",
			body:    `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`,
			wantChg: false,
		},
		{
			name:      "数组content提取text",
			body:      `{"model":"m1","messages":[{"role":"system","content":[{"type":"text","text":"A"},{"type":"image_url","image_url":{"url":"x"}},{"type":"text","text":"B"}]},{"role":"user","content":"hi"},{"role":"system","content":"C"}]}`,
			wantChg:   true,
			wantSys:   1,
			wantFirst: true,
			wantText:  "A\nB\n\nC",
		},
		{
			name:      "非system顺序保持",
			body:      `{"model":"m1","messages":[{"role":"user","content":"u1"},{"role":"system","content":"s1"},{"role":"assistant","content":"a"},{"role":"user","content":"u2"},{"role":"system","content":"s2"}]}`,
			wantChg:   true,
			wantSys:   1,
			wantFirst: true,
			wantText:  "s1\n\ns2",
		},
		{
			name:    "无messages字段不变",
			body:    `{"model":"m1","prompt":"x"}`,
			wantChg: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, changed := MergeSystemMessages([]byte(tc.body))
			if changed != tc.wantChg {
				t.Fatalf("changed = %v, want %v", changed, tc.wantChg)
			}
			if !tc.wantChg {
				return
			}
			msgs := gjson.GetBytes(out, "messages").Array()
			sysCnt := 0
			for i, m := range msgs {
				if m.Get("role").String() == "system" {
					sysCnt++
					if i != 0 {
						t.Errorf("system 不在第一位（index=%d）", i)
					}
					if tc.wantText != "" && m.Get("content").String() != tc.wantText {
						t.Errorf("content = %q, want %q", m.Get("content").String(), tc.wantText)
					}
				}
			}
			if sysCnt != tc.wantSys {
				t.Errorf("system 数量 = %d, want %d", sysCnt, tc.wantSys)
			}
		})
	}
}

// 端到端：多 system 请求经 ChatCompletions 转发后，上游收到合并后的单条 system
func TestChatCompletions_MergeSystemMessages(t *testing.T) {
	var received string
	upstream, h := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		received = string(data)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	})
	defer upstream.Close()

	rec := doChat(t, h, `{"model":"deepseek-v4-flash","messages":[{"role":"system","content":"S1"},{"role":"user","content":"u"},{"role":"system","content":"S2"}],"max_tokens":10}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(received, `"content":"S1\n\nS2"`) {
		t.Fatalf("上游收到的 body 未合并 system: %s", received)
	}
	if strings.Count(received, `"role":"system"`) != 1 {
		t.Fatalf("上游收到的 body 中 system 数量 != 1: %s", received)
	}
	first := gjson.GetBytes([]byte(received), "messages.0.role").String()
	if first != "system" {
		t.Fatalf("第一位不是 system: %s", received)
	}
}

func TestCleanOrphanToolMessages(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantChg bool
		wantMsg int    // 清理后 messages 条数
		wantID  string // 第一个 tool 消息的 tool_call_id（用于验证正常消息保留）
	}{
		{
			name:    "正常tool消息带id保留",
			body:    `{"model":"m","messages":[{"role":"user","content":"u"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"r"}]}`,
			wantChg: false,
			wantMsg: 3,
			wantID:  "call_1",
		},
		{
			name:    "孤儿tool消息被删除",
			body:    `{"model":"m","messages":[{"role":"user","content":"u"},{"role":"tool","content":"orphan"},{"role":"assistant","content":"a"}]}`,
			wantChg: true,
			wantMsg: 2,
			wantID:  "",
		},
		{
			name:    "空tool_call_id的tool消息被删",
			body:    `{"model":"m","messages":[{"role":"tool","tool_call_id":"","content":"empty-id"},{"role":"user","content":"u"}]}`,
			wantChg: true,
			wantMsg: 1,
			wantID:  "",
		},
		{
			name:    "混合：只删孤儿保留正常",
			body:    `{"model":"m","messages":[{"role":"tool","tool_call_id":"ok","content":"fine"},{"role":"tool","content":"bad"},{"role":"user","content":"u"}]}`,
			wantChg: true,
			wantMsg: 2,
			wantID:  "ok",
		},
		{
			name:    "无messages不动",
			body:    `{"model":"m","prompt":"x"}`,
			wantChg: false,
		},
		{
			name:    "大数组性能：大量消息仅一个孤儿",
			body:    `{"model":"m","messages":[{"role":"tool","tool_call_id":"kill","content":"x"},` + strings.Repeat(`{"role":"tool","tool_call_id":"keep_1","content":"r"},`, 1000) + `{"role":"assistant","content":"end"}]}`,
			wantChg: false,
			wantMsg: 1002,
			wantID:  "kill", // 首条带 id 的正常 tool 消息必须在最前保留
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, changed := CleanOrphanToolMessages([]byte(tc.body))
			if changed != tc.wantChg {
				t.Fatalf("changed = %v, want %v", changed, tc.wantChg)
			}
			msgs := gjson.GetBytes(out, "messages").Array()
			if len(msgs) != tc.wantMsg {
				t.Fatalf("messages 条数 = %d, want %d", len(msgs), tc.wantMsg)
			}
			// 验证没有任何孤儿 tool 消息残留
			for i, m := range msgs {
				if m.Get("role").String() == "tool" && m.Get("tool_call_id").String() == "" {
					t.Errorf("残留孤儿 tool 消息 at index=%d: %s", i, m.Raw)
				}
			}
			if tc.wantID != "" {
				// 验证第一个 tool 消息的 tool_call_id 保留
				for _, m := range msgs {
					if m.Get("role").String() == "tool" {
						if got := m.Get("tool_call_id").String(); got != tc.wantID {
							t.Errorf("第一个 tool 消息 tool_call_id = %q, want %q", got, tc.wantID)
						}
						break
					}
				}
			}
		})
	}
}

// 端到端：孤儿 tool 消息经 ChatCompletions 转发前被清理，上游不再收到缺 tool_call_id 的消息
// （修复 agnes 等 sglang 托管上游 strict schema 校验 400）
func TestChatCompletions_CleanOrphanToolMessages(t *testing.T) {
	var received string
	upstream, h := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		received = string(data)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	})
	defer upstream.Close()

	// 消息链中混入一条缺 tool_call_id 的孤儿 tool 消息（上游若收到会 400）
	rec := doChat(t, h, `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"},{"role":"tool","content":"orphan"},{"role":"assistant","content":"ok"}],"max_tokens":10}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	msgs := gjson.GetBytes([]byte(received), "messages").Array()
	for i, m := range msgs {
		if m.Get("role").String() == "tool" {
			t.Fatalf("上游不应收到任何孤儿(或缺id) tool 消息 at index=%d: %s", i, m.Raw)
		}
	}
}
