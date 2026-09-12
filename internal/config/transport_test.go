package config

import (
	"net/http"
	"testing"
)

// TestNewUpstreamTransport_Isolated 验证旁路请求用的 Transport 与全局 DefaultTransport
// 不是同一个对象——这正是 2026-09-12 agnes 锁步超时故障的根因（共享连接池里一条
// 被网络黑洞的 keepalive 连接把同 host 的全部上游一起拖垮）。
func TestNewUpstreamTransport_Isolated(t *testing.T) {
	tr := NewUpstreamTransport()
	if tr == nil {
		t.Fatal("NewUpstreamTransport 不应返回 nil")
	}
	if tr == http.DefaultTransport {
		t.Fatal("必须返回独立 Transport，不能复用 http.DefaultTransport 全局连接池")
	}
	// 每次都应是新实例，避免调用方之间互相污染连接池
	if tr2 := NewUpstreamTransport(); tr2 == tr {
		t.Fatal("每次调用应返回新 Transport 实例")
	}
}

// TestNewUpstreamTransport_Timeouts 验证关键参数已设置：
// DisableKeepAlives 每次请求新建连接，彻底消除复用到已死空闲连接的可能；
// 自定义 DialContext 禁用 HTTP/2，避免多路复用连接被单点黑洞拖垮。
func TestNewUpstreamTransport_Timeouts(t *testing.T) {
	tr := NewUpstreamTransport()
	if !tr.DisableKeepAlives {
		t.Error("DisableKeepAlives 应为 true：旁路探测必须每次新建连接，防止复用已死空闲连接")
	}
	if tr.TLSHandshakeTimeout <= 0 {
		t.Error("TLSHandshakeTimeout 应大于 0")
	}
	if tr.DialContext == nil {
		t.Error("DialContext 应设置（自定义 DialContext 会禁用 HTTP/2）")
	}
}
