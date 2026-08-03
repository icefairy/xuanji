// Package router 实现模型到上游组的静态路由匹配。
package router

import (
	"errors"
	"math/rand"
	"strings"

	"github.com/icefairy/xuanji/internal/config"
)

// ErrNoRoute 表示没有任何路由规则匹配请求的模型。
var ErrNoRoute = errors.New("router: no route for model")

// Router 根据配置中的 routing.rules 把模型匹配到上游组。
type Router struct {
	cfg       *config.Config
	upstreams map[string]*config.Upstream
}

// New 基于配置构建 Router，并建立上游名 → 上游的索引。
func New(cfg *config.Config) *Router {
	upstreams := make(map[string]*config.Upstream, len(cfg.Upstreams))
	for i := range cfg.Upstreams {
		u := &cfg.Upstreams[i]
		upstreams[u.Name] = u
	}
	return &Router{cfg: cfg, upstreams: upstreams}
}

// Route 按 routing.rules 的顺序查找第一条匹配 model 的规则，返回该规则命中的
// 上游列表（按配置顺序）与规则的策略（strategy）。
// 规则引用的上游名无效时跳过；整条规则的命中上游全部无效则继续匹配下一条规则。
// 排序由调用方按 strategy 执行（见 proxy.SelectCandidates），本函数只负责过滤。
func (r *Router) Route(model string) ([]*config.Upstream, string, error) {
	for i := range r.cfg.Routing.Rules {
		rule := &r.cfg.Routing.Rules[i]
		if !matchModel(rule.Model, model) {
			continue
		}
		var ups []*config.Upstream
		for _, name := range rule.Upstreams {
			if u, ok := r.upstreams[name]; ok {
				ups = append(ups, u)
			}
		}
		if len(ups) == 0 {
			continue
		}
		strategy := rule.Strategy
		if strategy == "" {
			strategy = r.cfg.Routing.DefaultStrategy
		}
		return ups, strategy, nil
	}
	return nil, "", ErrNoRoute
}

// MapModel 对 model 应用 upstream 的 model_mapping，返回上游真实模型名；
// 无映射或上游为 nil 时原样返回。
//
// 一对多映射：模型映射值用竖线 | 分隔多个真实模型名，随机选一个（零侵入改动）。
// 例：{"deepseek-v4-flash": "sensenova-6.7-flash-lite|deepseek-v4-flash"}
func (r *Router) MapModel(upstream *config.Upstream, model string) string {
	if upstream == nil {
		return model
	}
	if mapped, ok := upstream.ModelMapping[model]; ok {
		if parts := strings.Split(mapped, "|"); len(parts) > 1 {
			return parts[rand.Intn(len(parts))]
		}
		return mapped
	}
	return model
}

// matchModel 判断 model 是否匹配 pattern，pattern 中的 * 匹配任意字符序列。
func matchModel(pattern, model string) bool {
	pi, mi := 0, 0
	star, mark := -1, 0
	for mi < len(model) {
		switch {
		case pi < len(pattern) && pattern[pi] == model[mi]:
			pi++
			mi++
		case pi < len(pattern) && pattern[pi] == '*':
			star = pi
			mark = mi
			pi++
		case star >= 0:
			pi = star + 1
			mi = mark + 1
			mark++
		default:
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}