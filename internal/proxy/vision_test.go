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

// 兜底转发时必须裁掉图片**之前**的历史上下文，但图片之后的全部对话必须保留。
// 回归 1（2026-09-16）：agnes 系处理 24 万 token 整段会话首字节要 60~125 秒，
// 最后一条含图（本轮识图）时：保留 system + 最近 3 条，丢弃更早历史。
// 回归 1（2026-09-16）：agnes 系处理超大 context 首字节要 60~125 秒，
// 客户端 5 分钟超时后大面积 context canceled —— 故识图时要裁历史提速。
func TestTrimVisionFallbackBody_KeepsImageAndNeighbors(t *testing.T) {
	// 构造：system + 10 条长历史 + 含图 user 消息（最后一条）
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
	sb.WriteString(`]}`)
	body := []byte(sb.String())

	got := trimVisionFallbackBody(body)

	if int64(len(got)) >= int64(len(body))/2 {
		t.Fatalf("trimmed body not smaller enough: %d -> %d bytes", len(body), len(got))
	}
	msgs := gjson.GetBytes(got, "messages").Array()
	// system + 最近 3 条（含含图的最后一条）
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4 (system + 3 recent); got=%s", len(msgs), string(got))
	}
	if msgs[0].Get("role").String() != "system" {
		t.Errorf("messages[0].role = %q, want system", msgs[0].Get("role").String())
	}
	if !strings.Contains(msgs[len(msgs)-1].Raw, "image_url") {
		t.Errorf("last message should be the image, got %s", msgs[len(msgs)-1].Raw)
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

// 回归：图片出现在**历史**中（如上一轮 read 工具返回的图），而当前轮用户发的是
// 纯文本新消息时，新消息必须保留 —— 否则视觉上游只看到旧图片 + 旧描述，
// 反复描述同一张图，用户的新问题被静默丢弃（2026-09-28 实测，pi agent 场景）。
func TestTrimVisionFallbackBody_KeepsLatestUserMessage(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4-flash","messages":[` +
		`{"role":"system","content":"sys"},` +
		`{"role":"user","content":"历史问题"},` +
		`{"role":"toolResult","content":[{"type":"text","text":"Read image file"},{"type":"image","data":"QUFB"}]},` +
		`{"role":"assistant","content":"这是一张粉色的蛋"},` +
		`{"role":"user","content":"能不能做得生动一点"}` +
		`]}`)

	got := trimVisionFallbackBody(body)
	if !strings.Contains(string(got), "能不能做得生动一点") {
		t.Fatalf("latest user message lost after trim:\n%s", string(got))
	}
	if !strings.Contains(string(got), "QUFB") {
		t.Fatalf("image lost after trim:\n%s", string(got))
	}
}

// 根因回归（2026-09-28）：图片位于历史中时，其**之后**的全部对话必须保留。
//
// isMultimodalRequest 只看「请求里有没有图」，因此识图之后的每一轮请求都会
// 重复进入 vision 兜底路径。若裁剪只保留图片±1 邻居，用户后续每一轮的提问与
// 助手回复都会被反复丢弃，模型表现为「失忆」：只看得见图、记不住任何对话。
// 实测症状：模型答「我这边没有之前『宠物形象图片生成』的任务记录或上下文」。
func TestTrimVisionFallbackBody_KeepsEverythingAfterImage(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4-flash","messages":[` +
		`{"role":"system","content":"sys"},` +
		`{"role":"user","content":"生成宠物形象"},` +
		`{"role":"tool","tool_call_id":"c1","content":"Read image file"},` +
		`{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,QUFB"}}]},` +
		`{"role":"assistant","content":"这是粉色蛋"},` +
		`{"role":"user","content":"能不能做得生动一点"},` +
		`{"role":"assistant","content":"建议A"},` +
		`{"role":"user","content":"贪吃蛇两个形态"},` +
		`{"role":"assistant","content":"建议B"},` +
		`{"role":"user","content":"1、宠物形象用ai多生成几种"},` +
		`{"role":"assistant","content":"收到"},` +
		`{"role":"user","content":"继续啊"}` +
		`]}`)

	got := trimVisionFallbackBody(body)

	// 图片之后的每一轮对话都必须还在
	for _, want := range []string{
		"能不能做得生动一点", "建议A",
		"贪吃蛇两个形态", "建议B",
		"宠物形象用ai多生成几种", "收到",
		"继续啊",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("对话内容被裁掉，模型会失忆: %q 不在裁剪结果中\n%s", want, string(got))
		}
	}
	// 图片本身保留，裁剪仍然生效（图片之前的长历史被去掉）
	if !strings.Contains(string(got), "QUFB") {
		t.Errorf("image lost after trim:\n%s", string(got))
	}
	if strings.Contains(string(got), "生成宠物形象") {
		t.Log("提示：图片之前的历史仍在，裁剪未缩小体积（可接受，但不省 token）")
	}
}

// 判定语义：**只要请求里任何位置有图**即为多模态（扫全历史），
// 因为 deepseek-v4-flash 名下上游均为纯文本模型，收到任何 image_url 即 400。
func TestIsMultimodalRequest_AnyImageCounts(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			"历史含图 + 当前轮纯文本 -> 是图片请求（必须兜底，否则纯文本上游 400）",
			`{"model":"m","messages":[` +
				`{"role":"user","content":[{"type":"image_url","image_url":{"url":"u"}}]},` +
				`{"role":"assistant","content":"这是图"},` +
				`{"role":"user","content":"继续说说别的"}]}`,
			true,
		},
		{
			"历史含图(工具结果) + 当前轮纯文本 -> 是图片请求",
			`{"model":"m","messages":[` +
				`{"role":"tool","tool_call_id":"c1","content":"Read image"},` +
				`{"role":"user","content":[{"type":"image_url","image_url":{"url":"u"}}]},` +
				`{"role":"assistant","content":"这是图"},` +
				`{"role":"user","content":"换个话题"}]}`,
			true,
		},
		{
			"最后一条带图 -> 是图片请求",
			`{"model":"m","messages":[` +
				`{"role":"user","content":"先聊别的"},` +
				`{"role":"assistant","content":"好"},` +
				`{"role":"user","content":[{"type":"image_url","image_url":{"url":"u"}}]}]}`,
			true,
		},
		{
			"全程纯文本 -> 不是图片请求",
			`{"model":"m","messages":[` +
				`{"role":"user","content":"纯文本"},` +
				`{"role":"assistant","content":"好"},` +
				`{"role":"user","content":[{"type":"text","text":"还是文本"}]}]}`,
			false,
		},
		{
			"仅 system/tool 含图 -> 也算（任何位置有图即纯文本上游会 400）",
			`{"model":"m","messages":[` +
				`{"role":"system","content":[{"type":"image_url","image_url":{"url":"u"}}]},` +
				`{"role":"user","content":"hi"}]}`,
			true,
		},
	}
	for _, tc := range cases {
		if got := isMultimodalRequest([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: isMultimodalRequest = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// 历史含图时也必须兜底到 vision（不再直发文本上游）。
func TestVisionFallback_HistoryImageTriggersFallback(t *testing.T) {
	textUp, visionUp, h, textModels, visionModels := newVisionTestHandler(t, "flash")
	defer textUp.Close()
	defer visionUp.Close()

	body := `{"model":"deepseek-v4-flash","messages":[` +
		`{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]},` +
		`{"role":"assistant","content":"这是图"},` +
		`{"role":"user","content":"继续说说别的"}]}`

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
	// 最后一条不含图 -> 不裁剪，完整历史保留（避免失忆）
	if !strings.Contains(rec.Body.String(), "vision-1") {
		t.Errorf("expected vision response, got %s", rec.Body.String())
	}
}

// 核心回归（2026-09-28）：历史含图但**最后一条不含图**时，不得裁剪。
//
// 这是「失忆」的直接防线：agent 会话里图片作为工具结果永久留在历史中，
// 此后每轮请求都会因含图而进入 vision 兜底。若此时仍裁剪，用户后续对话会被
// 反复删掉，模型表现为失忆（实测模型答「我没有之前的任务记录或上下文」）。
func TestTrimVisionFallbackBody_NoTrimWhenHistoryImageOnly(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4-flash","messages":[` +
		`{"role":"system","content":"sys"},` +
		`{"role":"user","content":"生成宠物形象"},` +
		`{"role":"tool","tool_call_id":"c1","content":"Read image file"},` +
		`{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,QUFB"}}]},` +
		`{"role":"assistant","content":"这是粉色蛋"},` +
		`{"role":"user","content":"能不能做得生动一点"},` +
		`{"role":"assistant","content":"建议A"},` +
		`{"role":"user","content":"贪吃蛇两个形态"},` +
		`{"role":"assistant","content":"建议B"},` +
		`{"role":"user","content":"1、宠物形象用ai多生成几种"}]}`)

	got := trimVisionFallbackBody(body)
	if string(got) != string(body) {
		t.Fatalf("历史含图但最后一条非图时不应裁剪，却被改了：\n%s", string(got))
	}
	// 后续每轮对话都必须在
	for _, want := range []string{"生成宠物形象", "能不能做得生动一点", "贪吃蛇两个形态", "宠物形象用ai多生成几种"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("对话被裁掉，模型会失忆: %q", want)
		}
	}
}

// 识图时（最后一条含图）保留 system + 最近 3 条，丢弃更早历史。
func TestTrimVisionFallbackBody_KeepsLastThreeWhenImageLast(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4-flash","messages":[` +
		`{"role":"system","content":"sys"},` +
		`{"role":"user","content":"很久以前的问题1"},` +
		`{"role":"assistant","content":"很久以前1"},` +
		`{"role":"user","content":"上一轮问题"},` +
		`{"role":"assistant","content":"上一轮回答"},` +
		`{"role":"user","content":[{"type":"text","text":"看看这张图"},{"type":"image_url","image_url":{"url":"u"}}]}` +
		`]}`)

	got := trimVisionFallbackBody(body)
	msgs := gjson.GetBytes(got, "messages").Array()
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4 (system + last 3); got=%s", len(msgs), string(got))
	}
	if !strings.Contains(string(got), "上一轮问题") {
		t.Errorf("最近上下文应保留")
	}
	if strings.Contains(string(got), "很久以前的问题1") {
		t.Errorf("更早历史应被裁掉以提速")
	}
	if !strings.Contains(string(got), "image_url") {
		t.Errorf("图片必须保留")
	}
}

// 裁剪后若窗口内含 role=tool，必须保留其配对的 assistant tool_calls，
// 否则上游报 400「tool calls and tool results do not match」（实测 codebuddy）。
func TestTrimVisionFallbackBody_KeepsToolCallPairing(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4-flash","messages":[` +
		`{"role":"system","content":"sys"},` +
		`{"role":"user","content":"很早的问题"},` +
		`{"role":"assistant","content":"很早的回答"},` +
		`{"role":"user","content":"再看一张"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"c9","type":"function","function":{"name":"read","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"c9","content":"Read image file"},` +
		`{"role":"user","content":[{"type":"image_url","image_url":{"url":"u"}}]}` +
		`]}`)

	got := trimVisionFallbackBody(body)
	if !strings.Contains(string(got), "tool_calls") {
		t.Fatalf("配对保护失败：tool 结果在窗口内，其 assistant tool_calls 必须一并保留\n%s", string(got))
	}
	if !strings.Contains(string(got), `"tool_call_id":"c9"`) {
		t.Fatalf("tool 结果本身应保留\n%s", string(got))
	}
}
