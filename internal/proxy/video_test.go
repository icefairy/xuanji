package proxy

import (
	"testing"

	"github.com/icefairy/xuanji/internal/config"
)

// 含 video_url 的多模态请求体
const videoBody = `{"model":"qwen3.6:35b","messages":[{"role":"user","content":[{"type":"text","text":"分析这个视频"},{"type":"video_url","video_url":{"url":"https://example.com/video.mp4"}}]}]}`

// 纯文本请求体
const textBody = `{"model":"qwen3.6:35b","messages":[{"role":"user","content":"你好"}]}`

// 含 image_url 但无 video_url（应放行）
const imageBody = `{"model":"qwen3.6:35b","messages":[{"role":"user","content":[{"type":"text","text":"看看图"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`

func TestContainsVideoURL(t *testing.T) {
	if !containsVideoURL([]byte(videoBody)) {
		t.Fatal("videoBody 应检测到 video_url")
	}
	if containsVideoURL([]byte(textBody)) {
		t.Fatal("textBody 不应检测到 video_url")
	}
	if containsVideoURL([]byte(imageBody)) {
		t.Fatal("imageBody 不应检测到 video_url")
	}
	if containsVideoURL([]byte(`not json`)) {
		t.Fatal("非法 JSON 应返回 false")
	}
}

// 开关默认关闭（安全默认：视频流量大，需显式开启）。
func TestVideoPassThroughDefaultOff(t *testing.T) {
	h := &Handler{cfg: &config.Config{}}
	if h.cfg.Proxy.VideoPassThrough {
		t.Fatal("默认应关闭视频透传")
	}
}
