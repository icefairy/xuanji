// Package proxy developer 角色兼容：部分客户端（pi agent、Hermes、Claude Code 等）会发送
// OpenAI 新协议角色 messages[].role=developer，但商汤/基元律动等上游只认
// system/user/assistant/tool，收到 developer 直接 400。
// 网关侧在转发前把 role=developer 归一化为 role=system（系统设置开关，默认开启）。
package proxy

import (
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// normalizeDeveloperRole 把 messages 中 role=developer 归一化为 role=system。
// 返回修改后的 body 与是否发生修改（无 developer 角色时零开销，原样返回）。
func normalizeDeveloperRole(body []byte) ([]byte, bool) {
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return body, false
	}
	arr := msgs.Array()
	nb := body
	changed := false
	for i := range arr {
		if arr[i].Get("role").String() != "developer" {
			continue
		}
		var err error
		nb, err = sjson.SetBytes(nb, fmt.Sprintf("messages.%d.role", i), "system")
		if err != nil {
			continue // 单条失败不影响其余消息，跳过继续
		}
		changed = true
	}
	return nb, changed
}
