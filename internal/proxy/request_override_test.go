package proxy

import (
	"encoding/json"
	"testing"
)

func TestApplyRequestOverride_Empty(t *testing.T) {
	body := []byte(`{"model":"x","temperature":0.7}`)
	for _, ov := range []string{"", "{}", "  "} {
		got, changed := applyRequestOverride(body, ov)
		if changed {
			t.Fatalf("override %q expected unchanged, got changed", ov)
		}
		if string(got) != string(body) {
			t.Fatalf("override %q should return original body", ov)
		}
	}
}

func TestApplyRequestOverride_TopLevelScalar(t *testing.T) {
	body := []byte(`{"model":"x","temperature":0.7}`)
	got, changed := applyRequestOverride(body, `{"temperature":0.3}`)
	if !changed {
		t.Fatal("expected changed")
	}
	var m map[string]interface{}
	json.Unmarshal(got, &m)
	if m["temperature"] != float64(0.3) {
		t.Fatalf("temperature = %v, want 0.3", m["temperature"])
	}
	// 未覆盖键保留
	if m["model"] != "x" {
		t.Fatalf("model = %v, want x", m["model"])
	}
}

func TestApplyRequestOverride_NestedAdd(t *testing.T) {
	body := []byte(`{"model":"x"}`)
	got, changed := applyRequestOverride(body, `{"chat_template_kwargs":{"enable_thinking":false}}`)
	if !changed {
		t.Fatal("expected changed")
	}
	var m map[string]interface{}
	json.Unmarshal(got, &m)
	ct, ok := m["chat_template_kwargs"].(map[string]interface{})
	if !ok {
		t.Fatalf("chat_template_kwargs = %v, want object", m["chat_template_kwargs"])
	}
	if ct["enable_thinking"] != false {
		t.Fatalf("enable_thinking = %v, want false", ct["enable_thinking"])
	}
}

func TestApplyRequestOverride_NestedMerge(t *testing.T) {
	body := []byte(`{"model":"x","chat_template_kwargs":{"thinking_budget":500}}`)
	got, changed := applyRequestOverride(body, `{"chat_template_kwargs":{"enable_thinking":false}}`)
	if !changed {
		t.Fatal("expected changed")
	}
	var m map[string]interface{}
	json.Unmarshal(got, &m)
	ct, _ := m["chat_template_kwargs"].(map[string]interface{})
	// enable_thinking 被覆盖
	if ct["enable_thinking"] != false {
		t.Fatalf("enable_thinking = %v, want false", ct["enable_thinking"])
	}
	// thinking_budget 保留（合并语义）
	if ct["thinking_budget"] != float64(500) {
		t.Fatalf("thinking_budget = %v, want 500 (should preserve existing key)", ct["thinking_budget"])
	}
}

func TestApplyRequestOverride_InvalidJSON(t *testing.T) {
	body := []byte(`{"model":"x"}`)
	got, changed := applyRequestOverride(body, `{not-json`)
	if changed {
		t.Fatal("invalid JSON should not change")
	}
	if string(got) != string(body) {
		t.Fatal("invalid JSON should return original body")
	}
}

func TestApplyRequestOverride_NumberAndString(t *testing.T) {
	body := []byte(`{"temperature":0.9,"max_tokens":100}`)
	got, changed := applyRequestOverride(body, `{"temperature":0.3,"max_tokens":50}`)
	if !changed {
		t.Fatal("expected changed")
	}
	var m map[string]interface{}
	json.Unmarshal(got, &m)
	if m["temperature"] != float64(0.3) {
		t.Fatalf("temperature = %v, want 0.3", m["temperature"])
	}
	if m["max_tokens"] != float64(50) {
		t.Fatalf("max_tokens = %v, want 50", m["max_tokens"])
	}
}
