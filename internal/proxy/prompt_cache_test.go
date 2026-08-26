package proxy

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

// TestStripPromptCacheKey 验证 prompt_cache_key 剥离：存在时删除、不存在时原样返回。
func TestStripPromptCacheKey(t *testing.T) {
	base := `{"model":"dsflash","messages":[{"role":"user","content":"hi"}],"stream":true}`

	// 1. 带 prompt_cache_key → 剥离后字段消失，其余字段完整
	with := `{"model":"dsflash","messages":[{"role":"user","content":"hi"}],"prompt_cache_key":"abc123","stream":true}`
	nb, changed := stripPromptCacheKey([]byte(with))
	if !changed {
		t.Fatalf("expected changed=true for body containing prompt_cache_key")
	}
	if gjson.GetBytes(nb, "prompt_cache_key").Exists() {
		t.Fatalf("prompt_cache_key should be removed, got body=%s", nb)
	}
	// 其余字段保留
	if gjson.GetBytes(nb, "model").String() != "dsflash" {
		t.Fatalf("model should stay, got body=%s", nb)
	}
	if gjson.GetBytes(nb, "stream").Bool() != true {
		t.Fatalf("stream should stay, got body=%s", nb)
	}
	if !gjson.GetBytes(nb, "messages.0.content").Exists() {
		t.Fatalf("messages should stay, got body=%s", nb)
	}

	// 2. 不含该字段 → 原样、changed=false
	nb2, changed2 := stripPromptCacheKey([]byte(base))
	if changed2 {
		t.Fatalf("expected changed=false for body without prompt_cache_key")
	}
	if string(nb2) != base {
		t.Fatalf("body should be unchanged, got %s", nb2)
	}

	// 3. 非法 JSON → 不阻断、原样返回
	bad := []byte(`{not json`)
	nb3, changed3 := stripPromptCacheKey(bad)
	if changed3 || string(nb3) != string(bad) {
		t.Fatalf("invalid json should pass through unchanged, got changed=%v body=%s", changed3, nb3)
	}

	// 4. 结果仍为合法 JSON
	if !json.Valid(nb) {
		t.Fatalf("stripped body must be valid json, got %s", nb)
	}
}