package admin

import (
	"net/http"
	"strings"
	"time"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/proxy"
)

// Models 返回 OpenAI 兼容的模型列表（GET /v1/models）。
//
// 只返回「至少存在一个有效上游」的模型：上游被禁用 / 欠费（含模型级欠费）/
// 不支持该模型时，该模型对客户端实际不可用，列出来只会误导客户端缓存。
//
// 有效性判定只看配置层面（健康检查、快速失败、冷却等短时状态不参与），
// 否则上游短暂抖动会让模型忽隐忽现，客户端模型列表跟着闪。
func (h *Handler) Models(w http.ResponseWriter, _ *http.Request) {
	type modelObj struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	var models []modelObj
	if h.cfg == nil {
		writeJSON(w, map[string]interface{}{"object": "list", "data": models})
		return
	}
	// 先建名字索引，避免规则 × 上游的线性查找退化成 O(n²)。
	idx := make(map[string]*modelFilterUpstream, len(h.cfg.Upstreams))
	for i := range h.cfg.Upstreams {
		up := &h.cfg.Upstreams[i]
		idx[up.Name] = &modelFilterUpstream{up: up, modelArrear: make(map[string]bool)}
	}

	seen := make(map[string]bool)
	for _, rule := range h.cfg.Routing.Rules {
		if seen[rule.Model] || rule.Model == "" {
			continue
		}
		if !h.modelHasValidUpstream(idx, rule.Model, rule.Upstreams) {
			continue
		}
		seen[rule.Model] = true
		models = append(models, modelObj{
			ID:      rule.Model,
			Object:  "model",
			Created: time.Now().Unix(),
			OwnedBy: "xuanji",
		})
	}
	writeJSON(w, map[string]interface{}{
		"object": "list",
		"data":   models,
	})
}

// modelFilterUpstream 是模型列表过滤期间的轻量视图：上游 + 模型级欠费查询缓存
// （同一上游可能被多条规则命中，缓存一下减少加锁与重复查询）。
type modelFilterUpstream struct {
	up          *config.Upstream
	modelArrear map[string]bool
}

// modelHasValidUpstream 判断模型是否还有至少一个有效上游：
// 上游存在、已启用、非整上游欠费、支持该模型，且（按模型计费时）该模型未欠费。
// 上游名字在配置里找不到视为无效（不 panic）。
//
// 路由规则名可以带通配（如 "mimo-v2.5-tts*"），此时不能直接拿规则名去比对上游
// 的 models 列表——上游声明的是具体模型名（mimo-v2.5-tts / -voiceclone / -voicedesign）。
// 若按字面比对会误判「无上游支持」而把整个通配规则从列表里删掉，客户端就再也
// 看不到这组模型。这里用与 router.matchModel 相同的通配语义：规则名是通配时，
// 只要上游提供任意一个匹配的模型即视为有效。
func (h *Handler) modelHasValidUpstream(idx map[string]*modelFilterUpstream, model string, names []string) bool {
	for _, name := range names {
		v, ok := idx[name]
		if !ok || v.up == nil {
			continue
		}
		up := v.up
		if !up.Enabled || up.Arrears {
			continue
		}
		if !modelSatisfiedByUpstream(up, model) {
			continue
		}
		if up.PerModelBilling {
			arrear, queried := v.modelArrear[model]
			if !queried {
				// h.px 为 nil 时无法查模型级欠费，退化处理为「未欠费」。
				arrear = h.px != nil && h.px.IsModelArrear(up.Name, model)
				v.modelArrear[model] = arrear
			}
			if arrear {
				continue
			}
		}
		return true
	}
	return false
}

// modelSatisfiedByUpstream 判断上游是否为该（可能是通配的）模型名提供能力。
//
// 非通配名直接复用 proxy 的判定，保证与路由行为一致；通配名则在上游声明的
// 模型列表 / model_mapping 里找任意一个匹配项——语义与 router.matchModel 相同。
func modelSatisfiedByUpstream(up *config.Upstream, model string) bool {
	if !strings.Contains(model, "*") {
		return proxy.UpstreamSupportsModel(up, model)
	}
	// 未声明模型列表 = 全支持（与 UpstreamSupportsModel 保持一致）
	if len(up.Models) == 0 && len(up.ModelMapping) == 0 {
		return true
	}
	for _, m := range up.Models {
		if matchModelWildcard(model, m) {
			return true
		}
	}
	for k, v := range up.ModelMapping {
		if matchModelWildcard(model, k) {
			return true
		}
		for _, real := range strings.Split(v, "|") {
			if matchModelWildcard(model, strings.TrimSpace(real)) {
				return true
			}
		}
	}
	return false
}

// matchModelWildcard 是 router.matchModel 的最小等价实现（支持 * 通配）。
// 放在 admin 包内是因为该函数在 router 包未导出；语义必须与之一致，改动需同步。
func matchModelWildcard(pattern, model string) bool {
	if pattern == model {
		return true
	}
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
