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
//   - OpenAI o3/o4/gpt-5 系列：reasoning_effort 原生支持 → 透传。
package proxy

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
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
	case "kimi-k3":
		return applyKimiK3(body, effort)
	case "kimi-k2", "glm":
		return applySwitchOnly(body, effort)
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
	case strings.Contains(m, "kimi-k3"), strings.HasPrefix(m, "kimi-k3"):
		return "kimi-k3"
	case strings.Contains(m, "kimi-k2"):
		return "kimi-k2"
	case strings.Contains(m, "glm-4.5"), strings.Contains(m, "glm-4.6"):
		return "glm"
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
		nb, err = sjson.SetBytes(nb, "reasoning_effort", "max")
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
