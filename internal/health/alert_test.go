package health

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/icefairy/xuanji/internal/config"
)

// TestWebhookAlerter_Threshold 验证连续失败不足阈值不告警，达到阈值才告警一次。
func TestWebhookAlerter_Threshold(t *testing.T) {
	var mu sync.Mutex
	var received []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		received = append(received, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	a := NewWebhookAlerter(config.Alert{WebhookURL: srv.URL, ConsecutiveFails: 3})
	if a == nil {
		t.Fatal("alerter should be created")
	}

	// 连续 2 次失败（<3）不告警
	a.UpstreamStateChanged("u1", StateHealthy, StateHealthy, "x", 1)
	a.UpstreamStateChanged("u1", StateHealthy, StateHealthy, "x", 2)
	mu.Lock()
	n1 := len(received)
	mu.Unlock()
	if n1 != 0 {
		t.Fatalf("低于阈值不应告警, got %d", n1)
	}

	// 第 3 次失败（转入 degraded）→ 告警
	a.UpstreamStateChanged("u1", StateHealthy, StateDegraded, "timeout", 3)
	a.Wait(2 * time.Second)
	mu.Lock()
	n2 := len(received)
	mu.Unlock()
	if n2 != 1 {
		t.Fatalf("达到阈值应告警一次, got %d", n2)
	}

	// 同状态重复事件不重复告警（allow 冷却拦截）
	a.UpstreamStateChanged("u1", StateDegraded, StateDead, "timeout", 5)
	a.Wait(2 * time.Second)
	mu.Lock()
	n3 := len(received)
	mu.Unlock()
	if n3 < 2 {
		t.Fatalf("dead 事件应告警, got %d", n3)
	}

	// 恢复正常 → 恢复告警不受阈值限制
	a.UpstreamStateChanged("u1", StateDead, StateHealthy, "", 0)
	a.Wait(2 * time.Second)
	mu.Lock()
	n4 := len(received)
	mu.Unlock()
	if n4 != 3 {
		t.Fatalf("恢复应告警, got %d", n4)
	}
}

// TestWebhookAlerter_EmptyURL 空 WebhookURL 返回 nil（告警关闭）。
func TestWebhookAlerter_EmptyURL(t *testing.T) {
	if a := NewWebhookAlerter(config.Alert{}); a != nil {
		t.Fatal("空 WebhookURL 应返回 nil")
	}
}
