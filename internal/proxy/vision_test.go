package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/router"
	"github.com/tidwall/gjson"
)

// 含 image_url 的多模态请求体（OpenAI 风格）
const imageURLBody = `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":[{"type":"text","text":"看看这张图"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`

// 含 image 的多模态请求体（部分厂商风格：type=image）
const imageTypeBody = `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":[{"type":"image","image":"https://example.com/b.png"}]}]}`

// 纯文本请求体（content 为字符串，不算多模态）
const plainTextBody = `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"你好"}]}`

// 数组 content 但只有文本 part（不算多模态）
const textPartsBody = `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}`

func TestIsMultimodalRequest(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"image_url part", imageURLBody, true},
		{"image part", imageTypeBody, true},
		{"plain text string", plainTextBody, false},
		{"text-only parts array", textPartsBody, false},
		{"invalid json", `not json`, false},
		{"no messages", `{"model":"x"}`, false},
		{"messages not array", `{"model":"x","messages":{}}`, false},
		{"image in later message", `{"model":"x","messages":[{"role":"user","content":"text"},{"role":"user","content":[{"type":"image_url","image_url":{"url":"u"}}]}]}`, true},
	}
	for _, tc := range cases {
		if got := isMultimodalRequest([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: isMultimodalRequest = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// visionTestEnv 是 vision fallback 测试的双上游环境：
//   - text-up：提供纯文本模型 deepseek-v4-flash（vision=0），model_mapping 到真实名
//   - vision-up：提供多模态聚合模型 flash（vision=1），model_mapping 到真实名
//
// 每个上游把收到的真实模型名写入自己的 channel（buffered，无锁）。
func newVisionTestHandler(t *testing.T, fallback string) (textUp, visionUp *httptest.Server, h *Handler, textModels, visionModels chan string) {
	t.Helper()
	textModels = make(chan string, 4)
	visionModels = make(chan string, 4)

	textUp = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		textModels <- gjson.GetBytes(data, "model").String()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"text-1","object":"chat.completion","model":"x","choices":[{"index":0,"message":{"role":"assistant","content":"text-ok"},"finish_reason":"stop"}]}`)
	}))
	visionUp = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		visionModels <- gjson.GetBytes(data, "model").String()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"vision-1","object":"chat.completion","model":"x","choices":[{"index":0,"message":{"role":"assistant","content":"vision-ok"},"finish_reason":"stop"}]}`)
	}))

	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{
				Name:         "text-up",
				BaseURL:      textUp.URL + "/v1",
				APIKey:       "sk-text",
				Models:       []string{"deepseek-v4-flash"},
				ModelMapping: map[string]string{"deepseek-v4-flash": "deepseek-ai/DeepSeek-V4-Flash"},
			},
			{
				Name:         "vision-up",
				BaseURL:      visionUp.URL + "/v1",
				APIKey:       "sk-vision",
				Models:       []string{"flash"},
				ModelMapping: map[string]string{"flash": "qwen-vl-max"},
			},
		},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules: []config.Rule{
				// deepseek-v4-flash：vision=0（默认），fallback 由测试参数决定
				{Model: "deepseek-v4-flash", Upstreams: []string{"text-up"}, Strategy: "primary_backup", VisionFallback: fallback},
				// flash：vision=1（多模态聚合模型）
				{Model: "flash", Upstreams: []string{"vision-up"}, Strategy: "primary_backup", Vision: true},
			},
		},
	}
	h = New(cfg, router.New(cfg), nil)
	return textUp, visionUp, h, textModels, visionModels
}

// drain 非阻塞取出 buffered channel 里的全部值。
func drain(ch chan string) []string {
	var out []string
	for {
		select {
		case m := <-ch:
			out = append(out, m)
		default:
			return out
		}
	}
}

// 带图请求 + vision=0 + fallback="flash"：model 被改写并转发到多模态上游。
func TestVisionFallback_ImageRewritesModel(t *testing.T) {
	textUp, visionUp, h, textModels, visionModels := newVisionTestHandler(t, "flash")
	defer textUp.Close()
	defer visionUp.Close()

	rec := doChat(t, h, imageURLBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// 转发目标 = vision-up，收到的真实模型名由 model_mapping 映射
	if got := drain(visionModels); len(got) != 1 || got[0] != "qwen-vl-max" {
		t.Errorf("vision-up received models = %v, want [qwen-vl-max]", got)
	}
	if got := drain(textModels); len(got) != 0 {
		t.Errorf("text-up should not be hit, got %v", got)
	}
	// 响应是 vision-up 的（透传）
	if got := gjson.Get(rec.Body.String(), "id").String(); got != "vision-1" {
		t.Errorf("response id = %q, want vision-1", got)
	}
}

// 纯文本请求：不触发兜底，转发到原上游，行为与之前完全一致。
func TestVisionFallback_PlainTextNoRewrite(t *testing.T) {
	textUp, visionUp, h, textModels, visionModels := newVisionTestHandler(t, "flash")
	defer textUp.Close()
	defer visionUp.Close()

	rec := doChat(t, h, plainTextBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := drain(textModels); len(got) != 1 || got[0] != "deepseek-ai/DeepSeek-V4-Flash" {
		t.Errorf("text-up received models = %v, want [deepseek-ai/DeepSeek-V4-Flash]", got)
	}
	if got := drain(visionModels); len(got) != 0 {
		t.Errorf("vision-up should not be hit, got %v", got)
	}
	if got := gjson.Get(rec.Body.String(), "id").String(); got != "text-1" {
		t.Errorf("response id = %q, want text-1", got)
	}
}

// 带图请求直接命中 vision=1 的规则：不兜底，按原模型转发。
func TestVisionFallback_VisionEnabledRuleNoFallback(t *testing.T) {
	textUp, visionUp, h, textModels, visionModels := newVisionTestHandler(t, "flash")
	defer textUp.Close()
	defer visionUp.Close()

	// 客户端直接用多模态聚合模型 flash 发带图请求
	body := strings.Replace(imageURLBody, `"model":"deepseek-v4-flash"`, `"model":"flash"`, 1)
	rec := doChat(t, h, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := drain(visionModels); len(got) != 1 || got[0] != "qwen-vl-max" {
		t.Errorf("vision-up received models = %v, want [qwen-vl-max]", got)
	}
	if got := drain(textModels); len(got) != 0 {
		t.Errorf("text-up should not be hit, got %v", got)
	}
}

// 兜底转发时必须裁掉历史上下文：只保留 system + 含图消息 + 前后各一条。
// 回归：agnes 系上游处理 24 万 token 的整段会话首字节要 60~125 秒，
// 客户端 5 分钟超时后大面积 context canceled（2026-09-16 实测）。
func TestTrimVisionFallbackBody_KeepsImageAndNeighbors(t *testing.T) {
	// 构造：system + 10 条长历史 + 含图 user 消息 + 1 条尾部消息
	var sb strings.Builder
	sb.WriteString(`{"model":"deepseek-v4-flash","messages":[`)
	sb.WriteString(`{"role":"system","content":"you are helpful"},`)
	for i := 0; i < 10; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(fmt.Sprintf(`{"role":"user","content":"历史消息%d %s"}`, i, strings.Repeat("x", 200)))
	}
	sb.WriteString(`,` + `{"role":"user","content":[{"type":"text","text":"看看这张图"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}`)
	sb.WriteString(`,` + `{"role":"assistant","content":"尾部消息"}`)
	sb.WriteString(`]}`)
	body := []byte(sb.String())

	got := trimVisionFallbackBody(body)

	if int64(len(got)) >= int64(len(body))/2 {
		t.Fatalf("trimmed body not smaller enough: %d -> %d bytes", len(body), len(got))
	}
	msgs := gjson.GetBytes(got, "messages").Array()
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4 (system + prev neighbor + image + next neighbor); got=%s", len(msgs), string(got))
	}
	if msgs[0].Get("role").String() != "system" {
		t.Errorf("messages[0].role = %q, want system", msgs[0].Get("role").String())
	}
	if !strings.Contains(msgs[1].Raw, "历史消息9") {
		t.Errorf("messages[1] should be the previous neighbor, got %s", msgs[1].Raw)
	}
	if !strings.Contains(msgs[2].Raw, "image_url") {
		t.Errorf("messages[2] should be the image message, got %s", msgs[2].Raw)
	}
	if msgs[3].Get("content").String() != "尾部消息" {
		t.Errorf("messages[3] = %q, want the trailing neighbor", msgs[3].Get("content").String())
	}
	// 模型名等顶层字段必须保留
	if got2 := gjson.GetBytes(got, "model").String(); got2 != "deepseek-v4-flash" {
		t.Errorf("model = %q, want deepseek-v4-flash", got2)
	}
}

// 单条含图消息（无 system、无邻居）：原样保留，不误删。
func TestTrimVisionFallbackBody_SingleImageMessage(t *testing.T) {
	got := trimVisionFallbackBody([]byte(imageURLBody))
	msgs := gjson.GetBytes(got, "messages").Array()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1; got=%s", len(msgs), string(got))
	}
	if !strings.Contains(msgs[0].Raw, "image_url") {
		t.Errorf("image message lost: %s", msgs[0].Raw)
	}
}

// 非 JSON / 无 messages：原样返回，不 panic。
func TestTrimVisionFallbackBody_InvalidInput(t *testing.T) {
	for _, in := range []string{`not json`, `{"model":"x"}`, `{"model":"x","messages":{}}`} {
		if got := trimVisionFallbackBody([]byte(in)); string(got) != in {
			t.Errorf("trimVisionFallbackBody(%q) = %q, want unchanged", in, string(got))
		}
	}
}

// 带图请求 + vision=0 + fallback 空：不兜底，保持原行为（原样转发到 text-up）。
func TestVisionFallback_NoFallbackConfigured(t *testing.T) {
	textUp, visionUp, h, textModels, visionModels := newVisionTestHandler(t, "")
	defer textUp.Close()
	defer visionUp.Close()

	rec := doChat(t, h, imageURLBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := drain(textModels); len(got) != 1 || got[0] != "deepseek-ai/DeepSeek-V4-Flash" {
		t.Errorf("text-up received models = %v, want [deepseek-ai/DeepSeek-V4-Flash] (no rewrite)", got)
	}
	if got := drain(visionModels); len(got) != 0 {
		t.Errorf("vision-up should not be hit, got %v", got)
	}
}

// 端到端：兜底转发时上游收到的 body 必须已被裁剪（历史长尾不进 agnes）。
// 用独立环境（不做 model_mapping）以便断言 body 原文。
func TestVisionFallback_TrimsBodyBeforeForward(t *testing.T) {
	visionBodies := make(chan string, 4)
	textBodies := make(chan string, 4)
	resp := `{"id":"vision-1","object":"chat.completion","model":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`

	visionUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		visionBodies <- string(data)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, resp)
	}))
	textUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		textBodies <- string(data)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, resp)
	}))
	defer visionUp.Close()
	defer textUp.Close()

	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{Name: "text-up", BaseURL: textUp.URL + "/v1", APIKey: "sk-t", Models: []string{"deepseek-v4-flash"}},
			{Name: "vision-up", BaseURL: visionUp.URL + "/v1", APIKey: "sk-v", Models: []string{"flash"},
				ModelMapping: map[string]string{"flash": "agnes-2.5-flash"}},
		},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules: []config.Rule{
				{Model: "deepseek-v4-flash", Upstreams: []string{"text-up"}, Strategy: "primary_backup", VisionFallback: "flash"},
				{Model: "flash", Upstreams: []string{"vision-up"}, Strategy: "primary_backup", Vision: true},
			},
		},
	}
	h := New(cfg, router.New(cfg), nil)

	var sb strings.Builder
	sb.WriteString(`{"model":"deepseek-v4-flash","messages":[`)
	for i := 0; i < 20; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(fmt.Sprintf(`{"role":"user","content":"历史%d %s"}`, i, strings.Repeat("y", 500)))
	}
	sb.WriteString(`,` + `{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}`)
	sb.WriteString(`]}`)
	orig := sb.String()

	rec := doChat(t, h, orig)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	select {
	case got := <-visionBodies:
		if len(got) >= len(orig)/2 {
			t.Errorf("forwarded body not trimmed: %d -> %d bytes", len(orig), len(got))
		}
		if !strings.Contains(got, "image_url") {
			t.Errorf("forwarded body lost the image: %s", got)
		}
		if got2 := gjson.Get(got, "model").String(); got2 != "agnes-2.5-flash" {
			t.Errorf("forwarded model = %q, want agnes-2.5-flash (mapped)", got2)
		}
	default:
		t.Fatal("vision-up was not hit")
	}
	if got := drain(textBodies); len(got) != 0 {
		t.Errorf("text-up should not be hit, got %d requests", len(got))
	}
}
