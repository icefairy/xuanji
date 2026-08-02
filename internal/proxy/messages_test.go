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
