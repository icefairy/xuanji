package anthropic

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestClaudeRequestToOpenAI_ArrayContent 回归：Claude Code 的请求里
// system 是数组块、user content 是数组块——json.Unmarshal 到 any 后变成
// []interface{}，此前 switch 类型断言全部落空导致 messages=null、system 丢失，
// 上游 400、Claude Code 报"消息为空"（2026-08-02 修复）。
func TestClaudeRequestToOpenAI_ArrayContent(t *testing.T) {
	body := []byte(`{
		"model":"deepseek-v4-flash",
		"max_tokens":4096,
		"stream":false,
		"system":[{"type":"text","text":"You are Claude Code."}],
		"tools":[{"name":"Read","description":"Read a file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}],
		"messages":[{"role":"user","content":[{"type":"text","text":"Read sample.txt"}]}]
	}`)
	var req ClaudeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := ClaudeRequestToOpenAI(&req)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	s := string(out)
	// system 消息必须保留
	if !strings.Contains(s, `"role":"system"`) || !strings.Contains(s, "You are Claude Code") {
		t.Errorf("system message missing: %s", s)
	}
	// 用户消息内容必须保留
	if !strings.Contains(s, "Read sample.txt") {
		t.Errorf("user content missing: %s", s)
	}
	// messages 不能为 null
	if strings.Contains(s, `"messages":null`) {
		t.Errorf("messages is null: %s", s)
	}
	// tools 必须转换
	if !strings.Contains(s, `"type":"function"`) {
		t.Errorf("tools missing: %s", s)
	}
}

// TestClaudeRequestToOpenAI_StringContent 回归：纯字符串 content 与 system 保持兼容。
func TestClaudeRequestToOpenAI_StringContent(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"system":"plain system",
		"messages":[{"role":"user","content":"hello"}]
	}`)
	var req ClaudeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := ClaudeRequestToOpenAI(&req)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "plain system") || !strings.Contains(s, "hello") {
		t.Errorf("string content lost: %s", s)
	}
}
