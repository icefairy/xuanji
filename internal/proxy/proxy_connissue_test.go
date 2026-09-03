package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/health"
	"github.com/icefairy/xuanji/internal/router"
)

// TestIsConnIssue 验证连接类错误判定：客户端主动断连（context.Canceled）不算网络问题，
// 真实的网络故障（超时/连接拒绝/重置）才算。修复前 *url.Error（实现 net.Error）包装
// context.Canceled 会被 errors.As 误命中，导致客户端断连触发"全局网络问题"误判，
// 错误清空 fastfail 黑名单（2026-08-28 日志实测：cleared=7）。
func TestIsConnIssue(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"bare context.Canceled", context.Canceled, false},
		{"url.Error wrapping Canceled", &url.Error{Op: "Post", URL: "http://x/v1/chat/completions", Err: context.Canceled}, false},
		{
			// 完整错误链：forwardOnce 返回 fmt.Errorf("upstream request failed: %w", client.Do 的 *url.Error)
			"wrapped upstream request failed (canceled)",
			fmt.Errorf("upstream request failed: %w", &url.Error{Op: "Post", URL: "http://192.168.3.1:3004/v1/chat/completions", Err: context.Canceled}),
			false,
		},
		{"context.DeadlineExceeded", context.DeadlineExceeded, true},
		{"url.Error wrapping DeadlineExceeded", &url.Error{Op: "Post", URL: "http://x", Err: context.DeadlineExceeded}, true},
		{
			"connection refused",
			fmt.Errorf("upstream request failed: %w", &url.Error{Op: "Post", URL: "http://x", Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}}),
			true,
		},
		{
			"connection reset by peer",
			fmt.Errorf("upstream request failed: %w", &url.Error{Op: "Post", URL: "http://x", Err: errors.New("read tcp 1.2.3.4:5->1.2.3.1:3004: read: connection reset by peer")}),
			true,
		},
		{"plain upstream http error (429)", errors.New("upstream error: 429 Too Many Requests"), false},
		{"plain upstream http error (500)", errors.New("upstream error: 500 Internal Server Error"), false},
	}
	for _, tt := range tests {
		if got := isConnIssue(tt.err); got != tt.want {
			t.Errorf("%s: isConnIssue = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestChatCompletions_ClientDisconnectKeepsBlacklist 验证客户端断连（context canceled）
// 不清空 fastfail 黑名单：断连是客户端行为而非网络故障，黑名单应原样保留。
// 修复前：上游请求因 context canceled 失败 → *url.Error 误命中 net.Error →
// connIssues=true → "all upstreams failed with connection errors" 误清黑名单。
func TestChatCompletions_ClientDisconnectKeepsBlacklist(t *testing.T) {
	// 上游 handler 阻塞在测试可控 channel 上（模拟慢上游），
	// 客户端 context 取消后 client.Do 立即返回 canceled 错误，无需服务端配合。
	entered := make(chan struct{})
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered) // 通知测试：请求已到达上游
		<-release      // 阻塞直到测试收尾放行，避免 handler 泄漏卡住 up.Close()
	}))
	defer up.Close() // 后注册先执行：先放行 handler 再关 server
	defer close(release)

	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{
				Name: "up", BaseURL: up.URL, APIKey: "sk-test",
				Models:       []string{"real-model"},
				ModelMapping: map[string]string{"m": "real-model"},
			},
		},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules:           []config.Rule{{Model: "m", Upstreams: []string{"up"}, Strategy: "primary_backup"}},
		},
		Retry: config.Retry{MaxRetries: 10, RetryStatuses: []int{429, 500, 502, 503, 504}},
		// MaxRetries 放大：避免 up-conn1 快速失败在 cancel 触发前耗尽重试次数，
		// 导致循环“自然结束”而非“断连 aborted”——那会把测试变成非确定路径。
	}
	h := New(cfg, router.New(cfg), nil)
	ff := NewFastFailCache(time.Minute)
	h.SetFastFail(ff)

	// 预置黑名单：真实模型名被拉黑（模拟此前上游故障）
	ff.MarkFailed("up", "real-model")
	if !ff.IsBlacklisted("up", "real-model") {
		t.Fatal("precondition failed: blacklist entry should exist")
	}

	// 发起请求，在上游阻塞期间取消客户端 context（模拟客户端中途断连）
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	go func() {
		<-entered
		time.Sleep(20 * time.Millisecond) // 等请求进入阻塞
		cancel()
	}()
	h.ChatCompletions(rec, req)

	// 客户端断连不清空黑名单：真实模型仍应处于拉黑状态
	if !ff.IsBlacklisted("up", "real-model") {
		t.Error("fastfail blacklist entry was cleared on client disconnect (context.Canceled misclassified as conn issue)")
	}
}

// TestIsClientCanceled 验证客户端断连判定：错误链含 context.Canceled 即为客户端取消
// （*url.Error 包装后仍可被 errors.Is 穿透），与网络故障（reset/refused/timeout）区分开。
func TestIsClientCanceled(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"bare context.Canceled", context.Canceled, true},
		{
			// forwardOnce 实际返回形态：fmt.Errorf 包 *url.Error(context.Canceled)
			"upstream request failed (canceled)",
			fmt.Errorf("upstream request failed: %w", &url.Error{Op: "Post", URL: "http://x/v1/chat/completions", Err: context.Canceled}),
			true,
		},
		{"deadline exceeded is not cancel", context.DeadlineExceeded, false},
		{
			"connection reset is not cancel",
			fmt.Errorf("upstream request failed: %w", &url.Error{Op: "Post", URL: "http://x", Err: errors.New("connection reset by peer")}),
			false,
		},
	}
	for _, tt := range tests {
		if got := isClientCanceled(tt.err); got != tt.want {
			t.Errorf("%s: isClientCanceled = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestChatCompletions_ClientDisconnectKeepsHealth 验证客户端断连（context canceled）
// 不计入上游健康失败计数：断连是客户端行为而非上游故障，连续 2 次会把健康上游
// 误标 degraded（MarkFailure 一次即 degraded），导致后续请求被健康过滤误伤
// （2026-08-31 日志实测：10 例 canceled 使 bai/基元律动 fails 误增）。
// 修复前：循环里 MarkFailure 对任何 ferr 无条件调用，canceled 也计入。
func TestChatCompletions_ClientDisconnectKeepsHealth(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release // 阻塞直到测试收尾，客户端 cancel 后 Do 返回 canceled
	}))
	defer up.Close()
	defer close(release)

	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{Name: "up", BaseURL: up.URL, APIKey: "sk-test", Models: []string{"m"}},
		},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules:           []config.Rule{{Model: "m", Upstreams: []string{"up"}, Strategy: "primary_backup"}},
		},
		Retry: config.Retry{MaxRetries: 3, RetryStatuses: []int{429, 500, 502, 503, 504}},
	}
	hc := health.New(cfg)
	defer hc.Close()
	h := New(cfg, router.New(cfg), hc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	go func() {
		<-entered
		time.Sleep(5 * time.Millisecond)
		cancel() // 客户端断连
	}()
	h.ChatCompletions(rec, req)

	// 客户端断连不算上游故障：健康状态必须保持 healthy（未 degraded/dead）
	if st := hc.Status("up"); st != health.StateHealthy {
		t.Errorf("health status = %q, want healthy (client disconnect must not count as upstream failure)", st)
	}
}

// TestChatCompletions_ConnIssueThenCancelKeepsBlacklist 验证修复：
// 请求中已有真实连接类失败（connIssues=true，如 bai 超时）之后，
// 若因客户端断连（context canceled）提前 break 退出循环，
// 不得把"客户端取消"误判为"全部候选耗尽且全局网络故障"而清空 fastfail 黑名单
// （清空语义要求：循环自然走完，全部候选均尝试且失败）。
// 修复前：2026-09-02 日志实测 "upstream=dots ... context canceled" 后打出
// cleared=8 误清日志，故障上游黑名单被错误解除，下次请求立即重打故障上游。
func TestChatCompletions_ConnIssueThenBreakKeepsBlacklist(t *testing.T) {
	// up-conn：真实连接失败（端口无服务监听 → dial refused）→ connIssues=true
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := l.Addr().String()
	l.Close() // 立刻关闭：请求该地址必然 connection refused
	// up-block：阻塞式慢上游，客户端 cancel 后 client.Do 立即返回 canceled
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	upBlock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) }) // 循环重置后可能再次打 up-block，只 close 一次
		<-release
	}))
	defer upBlock.Close()
	defer close(release)

	cfg := &config.Config{
		Upstreams: []config.Upstream{
			// Weight=100 确保 up-conn1 总是候选第一（select 按 weight 降序）：
			// 必须先真实执行一次 dial（refused→fastfail 标记）再碰到阻塞上游，
			// 否则 cancel 后 client.Do 因 ctx 已取消直接返回、根本不拨号、
			// 不产生失败标记，测试无法复现"连接失败后断连"场景。
			{Name: "up-conn1", BaseURL: "http://" + deadAddr, APIKey: "x", Weight: 100},
			{Name: "up-block", BaseURL: upBlock.URL, APIKey: "sk-test", Weight: 1},
		},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules:           []config.Rule{{Model: "m", Upstreams: []string{"up-conn1", "up-block"}, Strategy: "primary_backup"}},
		},
		Retry: config.Retry{MaxRetries: 3, RetryStatuses: []int{429, 500, 502, 503, 504}},
	}
	h := New(cfg, router.New(cfg), nil)
	ff := NewFastFailCache(time.Minute)
	h.SetFastFail(ff)

	// 发起请求：up-1 快速连接失败（被 fastfail 标记），随后循环走向
	// up-block（阻塞中客户端取消）→ 循环因断连提前 break 退出。
	// 修复前：connIssues 已因 up-1 为 true → break 后外层误判
	// "全部候选耗尽+全局网络故障"清空黑名单 → up-1 黑名单被解除
	// （2026-09-02 生产日志实测：client disconnected 后 cleared=8 误清）。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	go func() {
		<-entered
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	h.ChatCompletions(rec, req)

	// 循环因客户端断连提前 break（候选未全部耗尽）：本次请求内被 fastfail
	// 标记的 up-conn1 必须保留在黑名单中，不能因"全局网络故障"误清
	// （2026-09-02 生产日志实测：client disconnected 后 cleared=8 误清）。
	if !ff.IsBlacklisted("up-conn1", "m") {
		t.Error("up-conn1 blacklist entry cleared after client disconnect (fastfail just marked in this request should persist)")
	}
}
