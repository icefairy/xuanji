// Package proxy 提供请求转发与上游选择。
package proxy

import (
	"strings"
	"sync"
	"time"
)

// FastFailCache 记录上游（渠道）或上游+模型组合的快速失败缓存。
// 上游失败后，在 cooldown 时间内直接跳过，不尝试连接。
// key 支持两级粒度：
//   - name+""        渠道级：整个上游被拉黑（连接错误、5xx 等）
//   - name+"::"+model 模型级：仅该渠道下的该模型被拉黑（配额用完、rate limit 等）
//
// 模型级 key 用于解决"同一渠道不同模型配额不同"的场景：
// 商汤日日新的 deepseek-v4-flash 5 小时 1500 次用完时，只跳过
// 商汤+deepseek-v4-flash，不影响商汤的 sensenova-u1-fast 等其他模型。
type FastFailCache struct {
	mu       sync.RWMutex
	entries  map[string]time.Time
	duration time.Duration
}

// NewFastFailCache 创建快速失败缓存。duration 为冷却时间。
func NewFastFailCache(duration time.Duration) *FastFailCache {
	return &FastFailCache{
		entries:  make(map[string]time.Time),
		duration: duration,
	}
}

// ffKey 构造缓存 key。model 为空表示渠道级。
func ffKey(name, model string) string {
	if model == "" {
		return name
	}
	return name + "::" + model
}

// MarkFailed 标记上游（或上游+模型）为失败状态。
// model 为空时标记整个渠道；model 非空时仅标记该渠道下的该模型。
func (f *FastFailCache) MarkFailed(name, model string) {
	f.mu.Lock()
	f.entries[ffKey(name, model)] = time.Now()
	f.mu.Unlock()
}

// MarkSuccess 清除上游（或上游+模型）的失败标记（成功恢复后调用）。
func (f *FastFailCache) MarkSuccess(name, model string) {
	f.mu.Lock()
	delete(f.entries, ffKey(name, model))
	f.mu.Unlock()
}

// IsBlacklisted 检查上游是否在冷却期内。
// 模型级检查优先：若 (name, model) 被标记则黑名单；否则回退到渠道级 name。
func (f *FastFailCache) IsBlacklisted(name, model string) bool {
	f.mu.RLock()
	failTime, ok := f.entries[ffKey(name, model)]
	if !ok && model != "" {
		failTime, ok = f.entries[name]
	}
	f.mu.RUnlock()
	if !ok {
		return false
	}
	if time.Since(failTime) >= f.duration {
		// 冷却期已过，清理
		f.mu.Lock()
		delete(f.entries, ffKey(name, model))
		if model != "" {
			delete(f.entries, name)
		}
		f.mu.Unlock()
		return false
	}
	return true
}

// Cleanup 清理所有已过期的条目。
func (f *FastFailCache) Cleanup() {
	f.mu.Lock()
	now := time.Now()
	for k, failTime := range f.entries {
		if now.Sub(failTime) >= f.duration {
			delete(f.entries, k)
		}
	}
	f.mu.Unlock()
}

// Name 是处于冷却期的上游或上游+模型组合。
type Name struct {
	Upstream string // 上游名
	Model    string // 模型名；空表示渠道级
}

// Names 返回当前处于冷却期（黑名单）的 (上游, 模型) 列表。
// 供后台探测任务遍历：对每个组合验证真实可用性，可用则解除，不可用则顺延。
func (f *FastFailCache) Names() []Name {
	f.mu.RLock()
	defer f.mu.RUnlock()
	now := time.Now()
	var out []Name
	for k, failTime := range f.entries {
		if now.Sub(failTime) >= f.duration {
			continue
		}
		// key 形如 "上游" 或 "上游::模型"
		if i := strings.Index(k, "::"); i >= 0 {
			out = append(out, Name{Upstream: k[:i], Model: k[i+2:]})
		} else {
			out = append(out, Name{Upstream: k})
		}
	}
	return out
}

// IsChannelBlacklisted 检查整个渠道是否被拉黑（兼容旧调用：不关心具体模型时用）。
func (f *FastFailCache) IsChannelBlacklisted(name string) bool {
	return f.IsBlacklisted(name, "")
}
