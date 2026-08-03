package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/icefairy/xuanji/internal/config"
)

// upstreamByName 从配置中按名称查找上游。
func (h *Handler) upstreamByName(name string) *config.Upstream {
	if h.cfg == nil {
		return nil
	}
	for i := range h.cfg.Upstreams {
		if h.cfg.Upstreams[i].Name == name {
			return &h.cfg.Upstreams[i]
		}
	}
	return nil
}

// probeModel 返回用于探测该上游的首选真实模型名（映射表的第一个映射值，兜底配置的第一个模型 / deepseek-v4-flash）。
// 注意返回的是真实模型名：一对多映射（竖线分隔）时取第一个；不再返回映射 key（客户端模型名），
// 否则探测请求会带上 "modelA|modelB" 这样的非法模型名。
func probeModel(u *config.Upstream) string {
	if u == nil {
		return "deepseek-v4-flash"
	}
	// 优先用映射表的第一个映射值（通常是该渠道最常用的真实模型）
	if len(u.ModelMapping) > 0 {
		for _, v := range u.ModelMapping {
			if v == "" {
				continue
			}
			if i := strings.Index(v, "|"); i >= 0 {
				return v[:i]
			}
			return v
		}
	}
	// 其次用配置的第一个模型
	if len(u.Models) > 0 {
		return u.Models[0]
	}
	return "deepseek-v4-flash"
}

// StartFastFailProbe 启动后台探测任务：定期检测 FastFailCache 中标记失败的上游，...
// 可用 → MarkSuccess 解除黑名单；不可用 → MarkFailed 刷新失败时间（顺延冷却）。
func (h *Handler) StartFastFailProbe(interval time.Duration) (stop func()) {
	stopCh := make(chan struct{})
	ticker := time.NewTicker(interval)
	go func() {
		for {
			select {
			case <-ticker.C:
				h.probeFastFailOnce()
			case <-stopCh:
				ticker.Stop()
				return
			}
		}
	}()
	// 首次启动也立即探测一次，不等间隔
	go h.probeFastFailOnce()
	return func() { close(stopCh) }
}

func (h *Handler) probeFastFailOnce() {
	if h.fastFail == nil {
		return
	}
	names := h.fastFail.Names()
	if len(names) == 0 {
		return
	}
	h.log.Info("fastfail probe", "count", len(names), "names", names)
	for _, n := range names {
		h.probeUpstream(n.Upstream, n.Model)
	}
}

func (h *Handler) probeUpstream(name, model string) {
	up := h.upstreamByName(name)
	if up == nil {
		return
	}
	// 构造最小 chat 探测请求。
	// model 参数来自 fastfail key，现在是映射后的真实模型名，直接使用；
	// 渠道级（model 为空）时用 probeModel 选一个真实模型名（返回的已是真实名，不再二次映射）。
	realModel := model
	if realModel == "" {
		realModel = probeModel(up)
	}
	body := map[string]any{
		"model":      realModel,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 1,
	}
	payload, _ := json.Marshal(body)

	target := strings.TrimRight(up.BaseURL, "/") + "/chat/completions"

	ctx, cancel := context.WithTimeout(context.Background(), upstreamTimeoutFor(h.cfg))
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if up.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+up.APIKey)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		h.log.Debug("fastfail probe error", "upstream", name, "model", model, "error", err)
		h.fastFail.MarkFailed(name, model) // 顺延
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		h.fastFail.MarkSuccess(name, model)
		h.log.Info("fastfail probe recovered", "upstream", name, "model", model)
	} else {
		h.fastFail.MarkFailed(name, model) // 顺延
		h.log.Debug("fastfail probe still down", "upstream", name, "model", model, "status", resp.StatusCode)
	}
}
