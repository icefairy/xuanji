package proxy

import (
	"encoding/json"
	"strings"

	"github.com/tidwall/sjson"
)

// applyRequestOverride 深度合并覆盖请求体：override 里的键强制盖掉请求体同名键，
// 嵌套对象逐层合并（未在 override 出现的子键保留请求体原值），请求体没有的键则新增。
// override 为空/空串/"{}" 或非法 JSON 时原样返回 false（不阻断转发、不报错）。
//
// 实现：把 override 递归展开成点路径（如 chat_template_kwargs.enable_thinking），
// 对每个叶子值用 sjson.SetBytes 写回请求体。setPath 闭包捕获外层 body 变量，
// 每次 Set 都读到最新 body，保证多次覆盖互相叠加。
func applyRequestOverride(body []byte, override string) ([]byte, bool) {
	override = strings.TrimSpace(override)
	if override == "" || override == "{}" {
		return body, false
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(override), &m); err != nil {
		return body, false // 非法 JSON：原样转发不阻断，不报错
	}
	changed := false
	var setPath func(prefix string, obj interface{})
	setPath = func(prefix string, obj interface{}) {
		switch v := obj.(type) {
		case map[string]interface{}:
			for k, sub := range v {
				key := k
				if prefix != "" {
					key = prefix + "." + k
				}
				setPath(key, sub)
			}
		default:
			nb, err := sjson.SetBytes(body, prefix, obj)
			if err == nil {
				body = nb
				changed = true
			}
		}
	}
	setPath("", m)
	return body, changed
}
