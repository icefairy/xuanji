// Package proxy 思考深度归一化：客户端统一用 OpenAI 标准 reasoning_effort，
// 网关按目标上游模型自动转换为该模型实际支持的思考控制参数。
//
// OpenAI 标准档位：none | minimal | low | medium | high | xhigh | max
//
// 各模型差异（2026-08-03 资料确认 + 实测）：
//   - DeepSeek V4 (flash/pro)：顶层 reasoning_effort 原生支持（low/high/xhigh/max），
//     关思考用 thinking.type=disabled。pro 的最低档映射为 high（官方映射表）。
//   - 商汤 sensenova-*：自定义 output_config.effort（low/medium/high），
//     关思考用 thinking.type=disabled 或 reasoning_effort=none（实测都有效）。
//   - Kimi K3：顶层 reasoning_effort 原生支持（low/high/max），始终思考不能关闭。
//   - Kimi K2.x：只用 thinking.type 开关（enabled/disabled + keep），无强度档。
//   - GLM-4.5：只用 thinking.type 开关（enabled/disabled），无强度档。
//   - Qwen3 / Qwen3.5 / Qwen3.6 / Qwen3.7：顶层 enable_thinking 开关 + thinking_budget 限思考长度，
//     无 reasoning_effort 档位（Qwen3.5 开源小模型默认禁用思考需显式开启；Qwen3.6 默认思考可关闭；
//     Qwen3 支持 /think /no_think 软切换，Qwen3.6 不支持）。Qwen2.5 无思考模式，不匹配本族。
//   - OpenAI o3/o4/gpt-5 系列：reasoning_effort 原生支持 → 透传。
package proxy

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/icefairy/xuanji/internal/config"
)

// normalizeThinkingEffort 将客户端标准 OpenAI 的 reasoning_effort 按目标上游模型转换为该模型实际参数。
// 返回转换后的 body 与是否发生过修改（body 未变时零开销，直接原样转发）。
// upstreamModel 是 model_mapping 后的上游真实模型名（最准），客户端模型名兜底。
func normalizeThinkingEffort(body []byte, upstreamModel string) ([]byte, bool) {
	if !gjson.GetBytes(body, "reasoning_effort").Exists() {
		return body, false
	}
	effort := gjson.GetBytes(body, "reasoning_effort").String()
	if effort == "" {
		return body, false
	}
	profile := matchThinkingProfile(upstreamModel)
	switch profile {
	case "deepseek":
		return applyDeepSeek(body, effort)
	case "sensenova":
		return applySenseNova(body, effort)
	case "agnes":
		return applyAgnes(body, effort)
	case "mimo":
		return applyMimo(body, effort)
	case "kimi-k3":
		return applyKimiK3(body, effort)
	case "kimi-k2", "glm":
		return applySwitchOnly(body, effort)
	case "qwen":
		return applyQwen(body, effort)
	default:
		// openai-native（o3/o4/gpt-5 等）与未知模型：原生支持 reasoning_effort，透传
		return body, false
	}
}

// matchThinkingProfile 根据模型名识别思考控制参数族。
// 规则：前缀/包含匹配，越具体越靠前。
func matchThinkingProfile(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "sensenova"):
		return "sensenova"
	case strings.Contains(m, "deepseek"):
		return "deepseek"
	case strings.Contains(m, "agnes"):
		return "agnes"
	case strings.Contains(m, "mimo"):
		return "mimo"
	case strings.Contains(m, "kimi-k3"), strings.HasPrefix(m, "kimi-k3"):
		return "kimi-k3"
	case strings.Contains(m, "kimi-k2"):
		return "kimi-k2"
	case strings.Contains(m, "glm-4.5"), strings.Contains(m, "glm-4.6"):
		return "glm"
	case strings.Contains(m, "qwen3"):
		// qwen3 / qwen3.5 / qwen3.6 / qwen3.7 / Qwen3-xxx（子串天然排除 qwen2.5，Qwen2.5 无思考模式）
		return "qwen"
	case strings.HasPrefix(m, "o3"), strings.HasPrefix(m, "o4"),
		strings.Contains(m, "gpt-5"):
		return "openai-native"
	default:
		return ""
	}
}

// applyDeepSeek DeepSeek V4：reasoning_effort 原生支持，只做档位适配与关思考转换。
// none → 删 reasoning_effort + thinking.type=disabled（实测关闭）
// minimal/low → low；medium → high（DeepSeek 默认 high≈OpenAI 默认 medium）
// high → high；xhigh/max → max；pro 模型 low 档抬到 high（官方映射表）
func applyDeepSeek(body []byte, effort string) ([]byte, bool) {
	nb, err := sjson.DeleteBytes(body, "reasoning_effort")
	if err != nil {
		return body, false
	}
	changed := true
	switch effort {
	case "none", "off":
		nb, err = sjson.SetBytes(nb, "thinking", map[string]string{"type": "disabled"})
		if err != nil {
			return body, false
		}
	case "minimal", "low":
		if strings.Contains(strings.ToLower(gjson.GetBytes(nb, "model").String()), "pro") {
			nb, err = sjson.SetBytes(nb, "reasoning_effort", "high")
		} else {
			nb, err = sjson.SetBytes(nb, "reasoning_effort", "low")
		}
		if err != nil {
			return body, false
		}
	case "medium":
		nb, err = sjson.SetBytes(nb, "reasoning_effort", "high")
		if err != nil {
			return body, false
		}
	case "high":
		nb, err = sjson.SetBytes(nb, "reasoning_effort", "high")
		if err != nil {
			return body, false
		}
	case "xhigh", "max":
		// 输出 xhigh 而非 max：部分挂 DeepSeek 名但用自己 API 的上游（如商汤把
		// deepseek-v4-flash 原样映射）枚举是 low/medium/high/xhigh/none，不认 max。
		// DeepSeek 官方与商汤都接受 xhigh，两边都不会 400。
		nb, err = sjson.SetBytes(nb, "reasoning_effort", "xhigh")
		if err != nil {
			return body, false
		}
	default: // 未知档位：原样透传（把删掉的加回来，视为未修改）
		return body, false
	}
	return nb, changed
}

// applySenseNova 商汤：reasoning_effort → output_config.effort，关思考用 thinking.type=disabled。
// none → 删 reasoning_effort + thinking.type=disabled（实测关闭）
// minimal/low → output_config.effort=low；medium → medium；high/xhigh/max → high
func applySenseNova(body []byte, effort string) ([]byte, bool) {
	nb, err := sjson.DeleteBytes(body, "reasoning_effort")
	if err != nil {
		return body, false
	}
	switch effort {
	case "none", "off":
		nb, err = sjson.SetBytes(nb, "thinking", map[string]string{"type": "disabled"})
		if err != nil {
			return body, false
		}
	case "minimal", "low":
		nb, err = sjson.SetBytes(nb, "output_config", map[string]string{"effort": "low"})
		if err != nil {
			return body, false
		}
	case "medium":
		nb, err = sjson.SetBytes(nb, "output_config", map[string]string{"effort": "medium"})
		if err != nil {
			return body, false
		}
	default: // high / xhigh / max / 未知 → 商汤最高档 high
		nb, err = sjson.SetBytes(nb, "output_config", map[string]string{"effort": "high"})
		if err != nil {
			return body, false
		}
	}
	return nb, true
}

// applyAgnes AgnesAI（sglang 托管）：reasoning_effort 原生支持，但枚举是
// none/low/medium/high/max（OpenAI 标准，**没有 xhigh**，实测 xhigh 直接 400）。
// 仅需把 xhigh 映射为 max；其余档位（none/low/medium/high/max）原样透传。
func applyAgnes(body []byte, effort string) ([]byte, bool) {
	if effort != "xhigh" {
		return body, false
	}
	nb, err := sjson.SetBytes(body, "reasoning_effort", "max")
	if err != nil {
		return body, false
	}
	return nb, true
}

// applyMimo MiMo（mimo-v2.5 / mimo-v2.5-pro 等）：reasoning_effort 原生支持，
// 但枚举只到 low/medium/high（实测 xhigh、max 原样透传→上游均 400 "Invalid request parameters"）。
// 因此把 xhigh/max 都降为 high（mimo 最高有效档），其余档位透传。
func applyMimo(body []byte, effort string) ([]byte, bool) {
	if effort != "xhigh" && effort != "max" {
		return body, false
	}
	nb, err := sjson.SetBytes(body, "reasoning_effort", "high")
	if err != nil {
		return body, false
	}
	return nb, true
}

// applyKimiK3 Kimi K3：顶层 reasoning_effort 原生支持（low/high/max），始终思考不能关闭。
// none → low（最接近"少想"的档位）；minimal/low→low；medium→high；high→high；xhigh/max→max
func applyKimiK3(body []byte, effort string) ([]byte, bool) {
	nb, err := sjson.DeleteBytes(body, "reasoning_effort")
	if err != nil {
		return body, false
	}
	var mapped string
	switch effort {
	case "none", "off", "minimal", "low":
		mapped = "low"
	case "medium", "high":
		mapped = "high"
	default: // xhigh / max / 未知
		mapped = "max"
	}
	nb, err = sjson.SetBytes(nb, "reasoning_effort", mapped)
	if err != nil {
		return body, false
	}
	return nb, true
}

// applySwitchOnly Kimi K2.x / GLM-4.5：只有 thinking.type 开关，无强度档。
// none → thinking.type=disabled；任何其他档位 → thinking.type=enabled；删 reasoning_effort。
func applySwitchOnly(body []byte, effort string) ([]byte, bool) {
	nb, err := sjson.DeleteBytes(body, "reasoning_effort")
	if err != nil {
		return body, false
	}
	typ := "enabled"
	if effort == "none" || effort == "off" {
		typ = "disabled"
	}
	nb, err = sjson.SetBytes(nb, "thinking", map[string]string{"type": typ})
	if err != nil {
		return body, false
	}
	return nb, true
}

// applyQwen Qwen3 系列（Qwen3/3.5/3.6/3.7）：顶层 enable_thinking 开关 + thinking_budget 限思考长度。
// 无 reasoning_effort 档位 → 档位映射为 enable_thinking + thinking_budget 分档：
//   none/off → enable_thinking=false（关闭思考）
//   minimal/low → enable_thinking=true + thinking_budget=1024（短思考，快响应）
//   medium → enable_thinking=true + thinking_budget=4096（中等）
//   high/xhigh/max → enable_thinking=true + thinking_budget=8192（深度思考）
//
// 说明：
//   - Qwen3.5 开源小模型默认禁用思考，high 档显式开启
//   - Qwen3.6 默认思考，none 档显式关闭
//   - Qwen3 支持 /think /no_think 提示词软切换，与 enable_thinking 正交，不做额外处理
func applyQwen(body []byte, effort string) ([]byte, bool) {
	nb, err := sjson.DeleteBytes(body, "reasoning_effort")
	if err != nil {
		return body, false
	}
	switch effort {
	case "none", "off":
		nb, err = sjson.SetBytes(nb, "enable_thinking", false)
	case "minimal", "low":
		nb, err = sjson.SetBytes(nb, "enable_thinking", true)
		if err == nil {
			nb, err = sjson.SetBytes(nb, "thinking_budget", 1024)
		}
	case "medium":
		nb, err = sjson.SetBytes(nb, "enable_thinking", true)
		if err == nil {
			nb, err = sjson.SetBytes(nb, "thinking_budget", 4096)
		}
	default: // high / xhigh / max / 未知 → 深度思考
		nb, err = sjson.SetBytes(nb, "enable_thinking", true)
		if err == nil {
			nb, err = sjson.SetBytes(nb, "thinking_budget", 8192)
		}
	}
	if err != nil {
		return body, false
	}
	return nb, true
}

// applyBestEffort 根据最佳思考等级配置与开关，决定请求最终的 reasoning_effort：
//   - auto 模式：客户端未传 reasoning_effort 且命中配置的推荐值 → 注入推荐值
//   - force 模式：客户端已传 reasoning_effort 且命中配置的强制值 → 覆盖为强制值
//
// 返回修改后的 body 与是否发生修改。修改后再交给 normalizeThinkingEffort 归一化。
// model 是客户端传入的模型名（effort_config 的 pattern 与客户端模型匹配）。
func applyBestEffort(body []byte, model string, cfg *config.Config) ([]byte, bool) {
	if !cfg.Proxy.AutoBestEffort && !cfg.Proxy.ForceBestEffort {
		return body, false
	}
	// 找第一条匹配的配置（越靠前优先级越高）
	var rec, forced string
	for i := range cfg.Proxy.EffortConfigs {
		e := &cfg.Proxy.EffortConfigs[i]
		if matchEffortPattern(e.Model, model) {
			rec, forced = e.Recommended, e.Forced
			break
		}
	}
	hasEffort := gjson.GetBytes(body, "reasoning_effort").Exists()
	if !hasEffort && cfg.Proxy.AutoBestEffort && rec != "" {
		nb, err := sjson.SetBytes(body, "reasoning_effort", rec)
		if err != nil {
			return body, false
		}
		return nb, true
	}
	if hasEffort && cfg.Proxy.ForceBestEffort && forced != "" {
		nb, err := sjson.SetBytes(body, "reasoning_effort", forced)
		if err != nil {
			return body, false
		}
		return nb, true
	}
	return body, false
}

// matchEffortPattern 判断 model 是否匹配 pattern（* 通配任意字符序列）。
func matchEffortPattern(pattern, model string) bool {
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
