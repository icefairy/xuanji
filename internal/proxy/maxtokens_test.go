package proxy

import (
	"strings"
	"testing"

	"github.com/icefairy/xuanji/internal/config"
)

func TestNormalizeMaxTokens_NoField(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	nb, changed := normalizeMaxTokens(body, nil)
	if changed || string(nb) != string(body) {
		t.Fatalf("no max_tokens should passthrough unchanged, changed=%v body=%s", changed, nb)
	}
}

func TestNormalizeMaxTokens_DeleteNonPositive(t *testing.T) {
	// max_tokens=0 → 删除字段
	body := []byte(`{"model":"x","max_tokens":0,"messages":[]}`)
	nb, changed := normalizeMaxTokens(body, nil)
	if !changed {
		t.Fatal("expected changed for max_tokens=0")
	}
	if strings.Contains(string(nb), "max_tokens") {
		t.Fatalf("max_tokens=0 should be deleted, got: %s", nb)
	}

	// 负数同样删除
	body = []byte(`{"model":"x","max_tokens":-5,"messages":[]}`)
	nb, changed = normalizeMaxTokens(body, nil)
	if !changed || strings.Contains(string(nb), "max_tokens") {
		t.Fatalf("max_tokens=-5 should be deleted, got: %s changed=%v", nb, changed)
	}
}

func TestNormalizeMaxTokens_ClampToCap(t *testing.T) {
	up := &config.Upstream{Name: "商汤", MaxTokensCap: 65536}
	// 超大值（deepseek 1M 窗口客户端按窗口填 131072）→ clamp 到 65536
	body := []byte(`{"model":"x","max_tokens":131072,"messages":[]}`)
	nb, changed := normalizeMaxTokens(body, up)
	if !changed {
		t.Fatal("expected changed for oversized max_tokens")
	}
	if !strings.Contains(string(nb), `"max_tokens":65536`) {
		t.Fatalf("max_tokens should clamp to 65536, got: %s", nb)
	}
}

func TestNormalizeMaxTokens_NoCap_DefaultClamp(t *testing.T) {
	// 未配置 cap（nil 或 cap=0）→ 默认 clamp 到 65536（绝大多数上游的安全上限）
	body := []byte(`{"model":"x","max_tokens":131072,"messages":[]}`)
	nb, changed := normalizeMaxTokens(body, nil)
	if !changed || !strings.Contains(string(nb), `"max_tokens":65536`) {
		t.Fatalf("nil cap should clamp to default 65536, changed=%v body=%s", changed, nb)
	}

	up := &config.Upstream{Name: "商汤", MaxTokensCap: 0}
	nb, changed = normalizeMaxTokens(body, up)
	if !changed || !strings.Contains(string(nb), `"max_tokens":65536`) {
		t.Fatalf("cap=0 should clamp to default 65536, changed=%v body=%s", changed, nb)
	}
}

func TestNormalizeMaxTokens_BiggerCap_Passthrough(t *testing.T) {
	// 上游配置了更大 cap（如 deepseek 官方大窗口）→ 不截断
	up := &config.Upstream{Name: "deepseek官方", MaxTokensCap: 1048576}
	body := []byte(`{"model":"x","max_tokens":131072,"messages":[]}`)
	nb, changed := normalizeMaxTokens(body, up)
	if changed || !strings.Contains(string(nb), `"max_tokens":131072`) {
		t.Fatalf("bigger cap should passthrough unchanged, changed=%v body=%s", changed, nb)
	}
}

func TestNormalizeMaxTokens_ValidValueUntouched(t *testing.T) {
	up := &config.Upstream{Name: "商汤", MaxTokensCap: 65536}
	body := []byte(`{"model":"x","max_tokens":4096,"messages":[]}`)
	nb, changed := normalizeMaxTokens(body, up)
	if changed || !strings.Contains(string(nb), `"max_tokens":4096`) {
		t.Fatalf("valid max_tokens should passthrough, changed=%v body=%s", changed, nb)
	}
}

func TestNormalizeMaxTokens_InvalidJSON(t *testing.T) {
	body := []byte(`not-json`)
	nb, changed := normalizeMaxTokens(body, nil)
	if changed || string(nb) != string(body) {
		t.Fatalf("invalid json should passthrough, changed=%v", changed)
	}
}
