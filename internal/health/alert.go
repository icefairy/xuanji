package health

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/icefairy/xuanji/internal/config"
)

// Alerter 接收上游健康状态变化事件。实现方负责节流与投递，不得阻塞调用方
// （checkOnce 持锁调用，阻塞会拖慢全部上游探测）。
type Alerter interface {
	// UpstreamStateChanged 在上游健康状态发生实质变化时调用。
	// from/to 为变化前后的状态；reason 为失败原因（恢复时为空）。
	UpstreamStateChanged(name string, from, to State, reason string, fails int)
}

// SetAlerter 注入告警器（应在 Start 之前调用）。
func (c *Checker) SetAlerter(a Alerter) { c.alerter = a }

// alertCooldown 是同一上游同类告警的最小间隔，防止抖动时刷屏。
const alertCooldown = 10 * time.Minute

// webhookPayload 是推送到告警 Webhook 的 JSON 结构。
// 字段同时包含结构化字段与可直接读的 text（兼容企业微信/钉钉/飞书等机器人的
// text 字段，也便于自建服务解析）。
type webhookPayload struct {
	Event     string `json:"event"` // upstream_degraded / upstream_dead / upstream_recovered
	Upstream  string `json:"upstream"`
	From      string `json:"from_state"`
	To        string `json:"to_state"`
	Fails     int    `json:"consecutive_fails"`
	Reason    string `json:"reason,omitempty"`
	Timestamp string `json:"timestamp"`
	Text      string `json:"text"` // 人类可读摘要（供机器人直接展示）
}

// WebhookAlerter 把健康状态变化推送到配置的 Webhook URL。
// 投递异步执行且带冷却，避免拖慢探测或故障抖动时刷屏。
type WebhookAlerter struct {
	url         string
	consecFails int
	client      *http.Client
	log         *slog.Logger

	mu       sync.Mutex
	lastSent map[string]time.Time // key = upstream + "|" + event
	pending  sync.WaitGroup       // 在途投递，Close 时等待（便于测试与优雅退出）
	closed   bool
}

// NewWebhookAlerter 基于配置创建告警器；WebhookURL 为空时返回 nil（表示关闭告警）。
func NewWebhookAlerter(cfg config.Alert) *WebhookAlerter {
	if cfg.WebhookURL == "" {
		return nil
	}
	fails := cfg.ConsecutiveFails
	if fails < 1 {
		fails = 3
	}
	return &WebhookAlerter{
		url:         cfg.WebhookURL,
		consecFails: fails,
		// 独立 Transport：告警目标与上游无关，但仍避免共享 DefaultTransport 全局池
		// （同样的黑洞复用问题，见 config.NewUpstreamTransport 注释）。
		client: &http.Client{
			Timeout:   10 * time.Second,
			Transport: config.NewUpstreamTransport(),
		},
		log:      slog.Default(),
		lastSent: map[string]time.Time{},
	}
}

// UpstreamStateChanged 实现 Alerter。仅在状态恶化为 degraded/dead、
// 或从异常恢复为 healthy 时投递；连续失败轮数不足 consecFails 时不告警。
func (a *WebhookAlerter) UpstreamStateChanged(name string, from, to State, reason string, fails int) {
	if a == nil || a.url == "" {
		return
	}
	if from == to {
		return
	}
	var event string
	switch {
	case to == StateDead:
		event = "upstream_dead"
	case to == StateDegraded:
		event = "upstream_degraded"
	case to == StateHealthy && (from == StateDead || from == StateDegraded):
		event = "upstream_recovered"
	default:
		return // 转为 unknown（禁用/欠费）等不算故障，不告警
	}
	// 恶化类告警需达到连续失败阈值；恢复类告警不受阈值限制（要尽快报喜）
	if event != "upstream_recovered" && fails < a.consecFails {
		return
	}
	if !a.allow(name, event) {
		return
	}

	p := webhookPayload{
		Event:     event,
		Upstream:  name,
		From:      string(from),
		To:        string(to),
		Fails:     fails,
		Reason:    reason,
		Timestamp: time.Now().Format(time.RFC3339),
	}
	p.Text = a.renderText(p)

	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.pending.Add(1)
	a.mu.Unlock()
	// 异步投递：checkOnce 持锁调用，绝不能阻塞探测循环。
	go func() {
		defer a.pending.Done()
		a.send(p)
	}()
}

// allow 判断该上游该事件是否已过冷却期（并发安全）。
func (a *WebhookAlerter) allow(name, event string) bool {
	key := name + "|" + event
	a.mu.Lock()
	defer a.mu.Unlock()
	if last, ok := a.lastSent[key]; ok && time.Since(last) < alertCooldown {
		return false
	}
	a.lastSent[key] = time.Now()
	return true
}

// renderText 生成人类可读的告警文案（中文，便于直接阅读）。
func (a *WebhookAlerter) renderText(p webhookPayload) string {
	switch p.Event {
	case "upstream_dead":
		return fmt.Sprintf("【璇玑告警】上游 %s 不可用（连续 %d 次探测失败）：%s", p.Upstream, p.Fails, p.Reason)
	case "upstream_degraded":
		return fmt.Sprintf("【璇玑告警】上游 %s 降级（连续 %d 次探测失败）：%s", p.Upstream, p.Fails, p.Reason)
	case "upstream_recovered":
		return fmt.Sprintf("【璇玑恢复】上游 %s 已恢复可用", p.Upstream)
	}
	return fmt.Sprintf("【璇玑告警】上游 %s 状态 %s → %s", p.Upstream, p.From, p.To)
}

// send 投递一次告警（失败仅记日志，不影响任何转发路径）。
func (a *WebhookAlerter) send(p webhookPayload) {
	b, err := json.Marshal(p)
	if err != nil {
		a.log.Warn("alert marshal failed", "error", err)
		return
	}
	req, err := http.NewRequest(http.MethodPost, a.url, bytes.NewReader(b))
	if err != nil {
		a.log.Warn("alert build request failed", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		a.log.Warn("alert webhook failed", "upstream", p.Upstream, "event", p.Event, "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		a.log.Warn("alert webhook non-2xx", "upstream", p.Upstream, "event", p.Event, "status", resp.StatusCode)
		return
	}
	a.log.Info("alert sent", "upstream", p.Upstream, "event", p.Event, "status", resp.StatusCode)
}

// Wait 等待在途告警投递完成（测试与优雅退出用）。超时后放弃等待。
func (a *WebhookAlerter) Wait(timeout time.Duration) {
	if a == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		a.pending.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}
