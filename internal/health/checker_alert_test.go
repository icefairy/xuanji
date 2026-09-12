package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/icefairy/xuanji/internal/config"
)

// fakeAlerter 记录收到的状态变化事件。
type fakeAlerter struct {
	mu   sync.Mutex
	got  []fakeAlert
	wait chan struct{} // 每收到一条就往里投递一次（测试同步用）
}

type fakeAlert struct {
	name     string
	from, to State
	reason   string
	fails    int
}

func newFakeAlerter() *fakeAlerter {
	return &fakeAlerter{wait: make(chan struct{}, 16)}
}

func (f *fakeAlerter) UpstreamStateChanged(name string, from, to State, reason string, fails int) {
	f.mu.Lock()
	f.got = append(f.got, fakeAlert{name, from, to, reason, fails})
	f.mu.Unlock()
	select {
	case f.wait <- struct{}{}:
	default:
	}
}

func (f *fakeAlerter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.got)
}

// TestChecker_AlertOnDegradeAndRecover 验证完整链路：
// 注入 alerter 后，上游 2 次失败（degraded）即触发一次告警，
// 恢复 healthy 再触发一次恢复告警；healthy→healthy 不打扰。
func TestChecker_AlertOnDegradeAndRecover(t *testing.T) {
	var fail atomic.Bool
	srv := startChatServer(t, &fail)
	defer srv.Close()

	c := New(testCfg(config.Upstream{
		Name:    "up",
		BaseURL: srv.URL,
		APIKey:  "sk-test",
		HealthCheck: &config.HealthCheck{
			Interval: config.Duration(time.Second),
			Timeout:  config.Duration(time.Second),
		},
	}))
	defer c.Close()

	al := newFakeAlerter()
	c.SetAlerter(al)

	ctx := context.Background()
	// 初始 healthy → healthy，不告警
	c.checkOnce(ctx, c.states["up"])
	if al.count() != 0 {
		t.Fatalf("healthy→healthy 不应告警, got %d", al.count())
	}

	// 第 1 次失败：仍 healthy（fails=1），不告警
	fail.Store(true)
	c.checkOnce(ctx, c.states["up"])
	if al.count() != 0 {
		t.Fatalf("1 次失败不应告警, got %d", al.count())
	}

	// 第 2 次失败：degraded，触发告警
	c.checkOnce(ctx, c.states["up"])
	select {
	case <-al.wait:
	case <-time.After(2 * time.Second):
		t.Fatal("degraded 未触发告警")
	}
	if al.count() != 1 {
		t.Fatalf("degraded 应恰好告警 1 次, got %d", al.count())
	}
	a := al.got[0]
	if a.from != StateHealthy || a.to != StateDegraded {
		t.Errorf("告警状态 = %s→%s, want healthy→degraded", a.from, a.to)
	}
	if a.reason == "" {
		t.Error("告警应带失败原因")
	}

	// 继续失败到 dead：dead 是一次新状态变化，应再告警
	// （之后连续失败不再重复告警，防刷屏）
	for i := 0; i < 3; i++ {
		c.checkOnce(ctx, c.states["up"])
	}
	countAtDead := al.count()
	if countAtDead != 2 {
		t.Fatalf("转 dead 后应告警（共 2 次）, got %d", countAtDead)
	}
	// 继续 dead → dead：不告警
	for i := 0; i < 3; i++ {
		c.checkOnce(ctx, c.states["up"])
	}
	if al.count() != countAtDead {
		t.Fatalf("dead→dead 不应告警, got %d", al.count())
	}

	// 恢复：dead → healthy，触发恢复告警
	fail.Store(false)
	c.checkOnce(ctx, c.states["up"])
	select {
	case <-al.wait:
	case <-time.After(2 * time.Second):
		t.Fatal("恢复未触发告警")
	}
	if al.count() != 3 {
		t.Fatalf("恢复后应告警（共 3 次）, got %d", al.count())
	}
	if a := al.got[2]; a.to != StateHealthy {
		t.Errorf("第 3 次告警应转为 healthy, got %s", a.to)
	}
}

// TestChecker_AlertViaWebhook 端到端：Checker + 真实 WebhookAlerter + 本地 Webhook 服务，
// 验证退化为 dead 时确实 POST 出 JSON。
func TestChecker_AlertViaWebhook(t *testing.T) {
	var fail atomic.Bool
	srv := startChatServer(t, &fail)
	defer srv.Close()

	var mu sync.Mutex
	got := map[string]bool{}
	wh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		buf := make([]byte, 512)
		n, _ := r.Body.Read(buf)
		mu.Lock()
		got[string(buf[:n])] = true
		mu.Unlock()
	}))
	defer wh.Close()

	c := New(testCfg(config.Upstream{
		Name:    "up",
		BaseURL: srv.URL,
		APIKey:  "sk-test",
		HealthCheck: &config.HealthCheck{
			Interval: config.Duration(time.Second),
			Timeout:  config.Duration(time.Second),
		},
	}))
	defer c.Close()
	c.SetAlerter(NewWebhookAlerter(config.Alert{WebhookURL: wh.URL, ConsecutiveFails: 2}))

	ctx := context.Background()
	fail.Store(true)
	for i := 0; i < 2; i++ { // 连续 2 次失败 → degraded ≥ 阈值 2
		c.checkOnce(ctx, c.states["up"])
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		saw := len(got) > 0
		mu.Unlock()
		if saw {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 {
		t.Fatal("webhook 未收到任何告警")
	}
	var jsonSeen bool
	for body := range got {
		if len(body) > 10 && len(body) < 1000 {
			jsonSeen = true
		}
	}
	if !jsonSeen {
		t.Fatalf("webhook 内容不像 JSON: %v", got)
	}
}
