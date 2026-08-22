package config

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestUpstreamUserAgent 验证全局 UA 注入：默认常量、Set/Get 与 Apply 到请求头。
func TestUpstreamUserAgent(t *testing.T) {
	// 默认常量与 pi 实际 UA 格式一致（pi/<ver> (platform; runtime; arch)）
	if DefaultUpstreamUserAgent == "" {
		t.Fatal("DefaultUpstreamUserAgent 不应为空")
	}

	// 未设置时 Apply 不写头（原子值为空）
	SetUpstreamUserAgent("")
	req := httptest.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", nil)
	ApplyUpstreamUserAgent(req)
	if got := req.Header.Get("User-Agent"); got != "" {
		t.Fatalf("空 UA 不应写入请求头，got %q", got)
	}

	// 设置后 Apply 写入
	expected := "pi/0.84.1 (linux; node/v22.22.1; x64)"
	SetUpstreamUserAgent(expected)
	req2 := httptest.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", nil)
	ApplyUpstreamUserAgent(req2)
	if got := req2.Header.Get("User-Agent"); got != expected {
		t.Fatalf("UA 不匹配，want %q got %q", expected, got)
	}

	// Hermes 预设同样可注入
	SetUpstreamUserAgent("hermes-cli/0.20.2")
	req3 := httptest.NewRequest(http.MethodPost, "http://upstream/v1/messages", nil)
	ApplyUpstreamUserAgent(req3)
	if got := req3.Header.Get("User-Agent"); got != "hermes-cli/0.20.2" {
		t.Fatalf("hermes UA 不匹配，got %q", got)
	}
	SetUpstreamUserAgent("")
	t.Cleanup(func() { SetUpstreamUserAgent("") })
}

// TestApplyUpstreamUserAgentOverwrite 验证覆盖既有头（Go 默认 UA 场景）。
func TestApplyUpstreamUserAgentOverwrite(t *testing.T) {
	SetUpstreamUserAgent("pi/0.84.1 (linux; node/v22.22.1; x64)")
	t.Cleanup(func() { SetUpstreamUserAgent("") })
	req := httptest.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", nil)
	req.Header.Set("User-Agent", "Go-http-client/1.1")
	ApplyUpstreamUserAgent(req)
	if got := req.Header.Get("User-Agent"); got != "pi/0.84.1 (linux; node/v22.22.1; x64)" {
		t.Fatalf("应覆盖默认 UA，got %q", got)
	}
}
