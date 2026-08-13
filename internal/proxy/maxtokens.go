package proxy

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/icefairy/xuanji/internal/config"
)

// defaultMaxTokensCap 是 max_tokens 的安全上限：绝大多数 OpenAI 兼容上游
// （商汤、基元律动等）要求 [1, 65536]，客户端按模型窗口自动填超大值会直接 400。
// 上游可通过 MaxTokensCap 配置更大值（如 deepseek 官方支持大窗口）。
const defaultMaxTokensCap = 65536

// normalizeMaxTokens 归一化请求体的 max_tokens：
//   - max_tokens <= 0（缺失/0/负数）：删除字段，让上游用默认值。
//     WorkBuddy 等客户端按模型窗口自动填 max_tokens 时可能出现 0 或超大值，
//     商汤/基元律动等上游要求 [1, 65536]，超范围直接 400。
//   - max_tokens > cap：clamp 到 cap。cap = 上游 MaxTokensCap（配置了且 >0 时）
//     否则默认 65536。这样未配置上游也能防 400，同时保留大窗口上游的扩展能力。
//
// 返回是否修改。
func normalizeMaxTokens(body []byte, up *config.Upstream) ([]byte, bool) {
	// 非 JSON 或无字段：不动
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body, false
	}
	v := gjson.GetBytes(body, "max_tokens")
	if !v.Exists() {
		return body, false
	}
	n := v.Int()
	if n <= 0 {
		nb, err := sjson.DeleteBytes(body, "max_tokens")
		if err != nil {
			return body, false
		}
		return nb, true
	}
	cap := int64(defaultMaxTokensCap)
	if up != nil && up.MaxTokensCap > 0 {
		cap = int64(up.MaxTokensCap)
	}
	if n > cap {
		nb, err := sjson.SetBytes(body, "max_tokens", cap)
		if err != nil {
			return body, false
		}
		return nb, true
	}
	return body, false
}
