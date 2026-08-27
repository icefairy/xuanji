package proxy

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

// TestStripPromptCacheParams 验证 prompt 缓存私有参数（prompt_cache_key /
// prompt_cache_retention）剥离：存在时删除、不存在时原样返回、两字段同时携带时全删。
func TestStripPromptCacheParams(t *testing.T) {
	base := `{"model":"dsflash","messages":[{"role":"user","content":"hi"}],"stream":true}`

	// 1. 带 prompt_cache_key → 剥离后字段消失，其余字段完整
	with := `{"model":"dsflash","messages":[{"role":"user","content":"hi"}],"prompt_cache_key":"abc123","stream":true}`
	nb, changed := stripPromptCacheParams([]byte(with))
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

	// 2. 带 prompt_cache_retention → 同样剥离（对所有上游无条件生效，
	//    聚合类上游对未知字段严格校验收到即 400 UNKNOWN_FIELD）
	withRetention := `{"model":"dsflash","messages":[{"role":"user","content":"hi"}],"prompt_cache_retention":"24h"}`
	nbR, changedR := stripPromptCacheParams([]byte(withRetention))
	if !changedR {
		t.Fatalf("expected changed=true for body containing prompt_cache_retention")
	}
	if gjson.GetBytes(nbR, "prompt_cache_retention").Exists() {
		t.Fatalf("prompt_cache_retention should be removed, got body=%s", nbR)
	}

	// 3. 两字段同时携带 → 全部删除
	both := `{"model":"m","prompt_cache_key":"k","prompt_cache_retention":"24h","messages":[]}`
	nbB, changedB := stripPromptCacheParams([]byte(both))
	if !changedB {
		t.Fatalf("expected changed=true for body containing both fields")
	}
	if gjson.GetBytes(nbB, "prompt_cache_key").Exists() || gjson.GetBytes(nbB, "prompt_cache_retention").Exists() {
		t.Fatalf("both fields should be removed, got body=%s", nbB)
	}

	// 4. 不含这些字段 → 原样、changed=false
	nb2, changed2 := stripPromptCacheParams([]byte(base))
	if changed2 {
		t.Fatalf("expected changed=false for body without cache params")
	}
	if string(nb2) != base {
		t.Fatalf("body should be unchanged, got %s", nb2)
	}

	// 5. 非法 JSON → 不阻断、原样返回
	bad := []byte(`{not json`)
	nb3, changed3 := stripPromptCacheParams(bad)
	if changed3 || string(nb3) != string(bad) {
		t.Fatalf("invalid json should pass through unchanged, got changed=%v body=%s", changed3, nb3)
	}

	// 6. 结果仍为合法 JSON
	if !json.Valid(nb) {
		t.Fatalf("stripped body must be valid json, got %s", nb)
	}
}
