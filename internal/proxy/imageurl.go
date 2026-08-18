// Package proxy 图片格式兼容：部分客户端（Hermes、OpenAI SDK 等）按 OpenAI 标准
// 发送嵌套格式 {"type":"image_url","image_url":{"url":"data:..."}}，但 vllm/
// agnes 等上游只认平铺格式 {"type":"image_url","image_url":"data:..."}，
// 收到嵌套对象直接 400 "Unexpected item type in content"。
// 网关侧在转发前把嵌套格式拍平为字符串格式（默认开启，无需配置）。
package proxy

import (
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// normalizeImageURLFlat 把 messages 中 image_url 的嵌套对象形式拍平为字符串形式：
//   {"type":"image_url","image_url":{"url":"DATA_URI"}}     → {"type":"image_url","image_url":"DATA_URI"}
//   {"type":"image_url","image_url":"DATA_URI"}             → 不动（已是平铺）
//   {"type":"image","image_url":{...}} / {"type":"image","image_url":"..."} → 同样处理（image 类型）
//
// 返回修改后的 body 与是否发生修改（无 image_url 时零开销，原样返回）。
func normalizeImageURLFlat(body []byte) ([]byte, bool) {
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return body, false
	}
	arr := msgs.Array()
	nb := body
	changed := false
	for i := range arr {
		content := arr[i].Get("content")
		if !content.IsArray() {
			continue
		}
		parts := content.Array()
		for j := range parts {
			t := parts[j].Get("type").String()
			if t != "image_url" && t != "image" {
				continue
			}
			iu := parts[j].Get("image_url")
			// 已经是字符串：不动
			if iu.Type == gjson.String {
				continue
			}
			// 不是对象（缺失/null 等）：跳过
			if !iu.IsObject() {
				continue
			}
			url := iu.Get("url").String()
			if url == "" {
				continue
			}
			var err error
			nb, err = sjson.SetBytes(nb, fmt.Sprintf("messages.%d.content.%d.image_url", i, j), url)
			if err != nil {
				continue // 单条失败不影响其余，跳过继续
			}
			changed = true
		}
	}
	return nb, changed
}