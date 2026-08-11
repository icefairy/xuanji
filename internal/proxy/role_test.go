package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// normalizeRole 调用 normalizeDeveloperRole 并返回转换后的 body 字符串。
func normalizeRole(t *testing.T, body string) (string, bool) {
	t.Helper()
	nb, changed := normalizeDeveloperRole([]byte(body))
	if nb == nil {
		t.Fatalf("normalizeDeveloperRole returned nil body")
	}
	return string(nb), changed
}

func TestNormalizeDeveloperRole_DeveloperToSystem(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"developer","content":"你是一个助手"}]}`
	nb, changed := normalizeRole(t, body)
	if !changed {
		t.Fatalf("expected changed=true")
	}
	if got := gjson.Get(nb, "messages.0.role").String(); got != "system" {
		t.Fatalf("messages.0.role=%q, want system (body=%s)", got, nb)
	}
	if got := gjson.Get(nb, "messages.0.content").String(); got != "你是一个助手" {
		t.Fatalf("content lost: %q (body=%s)", got, nb)
	}
	if got := gjson.Get(nb, "model").String(); got != "m" {
		t.Fatalf("model lost: %q (body=%s)", got, nb)
	}
}

func TestNormalizeDeveloperRole_NoDeveloper_NoChange(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}]}`
	nb, changed := normalizeRole(t, body)
	if changed {
		t.Fatalf("no developer role should not change body, got changed=true")
	}
	if string(nb) != body {
		t.Fatalf("body should be identical, got %s", nb)
	}
}

func TestNormalizeDeveloperRole_MultiMessages_OnlyDeveloperChanged(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"developer","content":"d1"},{"role":"user","content":"u1"},{"role":"developer","content":"d2"},{"role":"assistant","content":"a1"}]}`
	nb, changed := normalizeRole(t, body)
	if !changed {
		t.Fatalf("expected changed=true")
	}
	want := []string{"system", "user", "system", "assistant"}
	for i, w := range want {
		if got := gjson.Get(nb, fmt.Sprintf("messages.%d.role", i)).String(); got != w {
			t.Fatalf("messages.%d.role=%q, want %q (body=%s)", i, got, w, nb)
		}
	}
	// 其他字段不被破坏
	if got := gjson.Get(nb, "messages.1.content").String(); got != "u1" {
		t.Fatalf("messages.1.content lost: %q", got)
	}
	if got := gjson.Get(nb, "messages.3.content").String(); got != "a1" {
		t.Fatalf("messages.3.content lost: %q", got)
	}
}

func TestNormalizeDeveloperRole_NoMessages_NoChange(t *testing.T) {
	// 无 messages 数组：不修改
	body := `{"model":"m","stream":true}`
	nb, changed := normalizeRole(t, body)
	if changed {
		t.Fatalf("no messages should not change body, got changed=true")
	}
	if string(nb) != body {
		t.Fatalf("body should be identical, got %s", nb)
	}
}

func TestNormalizeDeveloperRole_InvalidJSON_NoChange(t *testing.T) {
	body := `{"model":"m","messages":`
	nb, changed := normalizeRole(t, body)
	if changed {
		t.Fatalf("invalid json should not change body, got changed=true")
	}
	if string(nb) != body {
		t.Fatalf("body should be identical, got %s", nb)
	}
}

// TestNormalizeDeveloperRole_KeepsOtherFields 确保归一化不破坏其他请求字段。
func TestNormalizeDeveloperRole_KeepsOtherFields(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"developer","content":"d"}],"temperature":0.7,"max_tokens":4096,"stream":true}`
	nb, changed := normalizeRole(t, body)
	if !changed {
		t.Fatalf("expected changed=true")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(nb), &m); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if m["temperature"] != 0.7 {
		t.Fatalf("temperature lost: %v", m["temperature"])
	}
	if !strings.Contains(nb, `"max_tokens":4096`) {
		t.Fatalf("max_tokens lost: %s", nb)
	}
	if got := gjson.Get(nb, "stream").Bool(); !got {
		t.Fatalf("stream lost: %s", nb)
	}
}
