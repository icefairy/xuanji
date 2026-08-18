package gemini

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestOpenAIChatToGemini 验证 OpenAI → Gemini 请求转换。
func TestOpenAIChatToGemini(t *testing.T) {
	body := `{
	  "model": "some-gemini-model",
	  "messages": [
	    {"role": "system", "content": "你是助手"},
	    {"role": "user", "content": [{"type": "text", "text": "你好"}, {"type": "image_url", "image_url": {"url": "data:image/png;base64,aGVsbG8="}}]},
	    {"role": "assistant", "content": "收到", "tool_calls": [{"id": "get_weather", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"北京\"}"}}]},
	    {"role": "tool", "tool_call_id": "get_weather", "content": "{\"temp\":25}"}
	  ],
	  "max_tokens": 100,
	  "temperature": 0.5,
	  "tools": [{"type": "function", "function": {"name": "get_weather", "description": "查天气", "parameters": {"type": "object"}}}]
	}`
	out, err := OpenAIChatToGemini([]byte(body), "upstream-real-model")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var g map[string]any
	if err := json.Unmarshal(out, &g); err != nil {
		t.Fatalf("output not json: %v", err)
	}
	if g["model"] != "upstream-real-model" {
		t.Errorf("model = %v, want upstream-real-model", g["model"])
	}
	sys, ok := g["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatalf("systemInstruction missing: %v", g)
	}
	sparts := sys["parts"].([]any)
	if sparts[0].(map[string]any)["text"] != "你是助手" {
		t.Errorf("system text = %v", sparts)
	}
	contents := g["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("contents len = %d, want 3 (user, model, user-tool)", len(contents))
	}
	// user: text + inlineData 图片
	u := contents[0].(map[string]any)
	uparts := u["parts"].([]any)
	if len(uparts) != 2 {
		t.Fatalf("user parts = %d", len(uparts))
	}
	img := uparts[1].(map[string]any)
	inline := img["inlineData"].(map[string]any)
	if inline["mimeType"] != "image/png" || inline["data"] != "aGVsbG8=" {
		t.Errorf("inlineData = %v", inline)
	}
	// assistant: text + functionCall
	m := contents[1].(map[string]any)
	mparts := m["parts"].([]any)
	if len(mparts) != 2 {
		t.Fatalf("model parts = %d", len(mparts))
	}
	fc := mparts[1].(map[string]any)["functionCall"].(map[string]any)
	if fc["name"] != "get_weather" {
		t.Errorf("functionCall = %v", fc)
	}
	// tool 消息 → functionResponse
	tr := contents[2].(map[string]any)
	fr := tr["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
	if fr["name"] != "get_weather" {
		t.Errorf("functionResponse = %v", fr)
	}
	// tools → functionDeclarations
	tools := g["tools"].([]any)
	decls := tools[0].(map[string]any)["functionDeclarations"].([]any)
	if decls[0].(map[string]any)["name"] != "get_weather" {
		t.Errorf("functionDeclarations = %v", decls)
	}
	// generationConfig
	gc := g["generationConfig"].(map[string]any)
	if gc["maxOutputTokens"] != float64(100) || gc["temperature"] != 0.5 {
		t.Errorf("generationConfig = %v", gc)
	}
}

// TestGeminiResponseToOpenAI 验证 Gemini → OpenAI 非流式响应转换。
func TestGeminiResponseToOpenAI(t *testing.T) {
	body := `{
	  "candidates": [{
	    "content": {"role": "model", "parts": [{"text": "答案"}, {"functionCall": {"name": "f", "args": {"a": 1}}}]},
	    "finishReason": "STOP"
	  }],
	  "usageMetadata": {"promptTokenCount": 10, "candidatesTokenCount": 5, "totalTokenCount": 15}
	}`
	out, err := GeminiResponseToOpenAI([]byte(body), "gemini-model")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("output not json: %v", err)
	}
	choices := resp["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %d", len(choices))
	}
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "答案" {
		t.Errorf("content = %v", msg["content"])
	}
	tcs := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls = %d", len(tcs))
	}
	tc := tcs[0].(map[string]any)
	if tc["function"].(map[string]any)["name"] != "f" {
		t.Errorf("tool_call = %v", tc)
	}
	if choices[0].(map[string]any)["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v", choices[0].(map[string]any)["finish_reason"])
	}
	usage := resp["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(10) || usage["completion_tokens"] != float64(5) {
		t.Errorf("usage = %v", usage)
	}
}

// TestStreamGeminiToOpenAI 验证 Gemini 流（SSE 与逐行 JSON 两种）→ OpenAI SSE。
func TestStreamGeminiToOpenAI(t *testing.T) {
	// Gemini 流式响应：逐行 JSON（官方 streamGenerateContent 默认格式）
	input := "{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"你\"}]}}]}\n" +
		"{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"好\"}]}}]}\n" +
		"{\"candidates\":[{\"content\":{\"role\":\"model\"},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":3,\"totalTokenCount\":8}}\n"

	var sb strings.Builder
	err := StreamGeminiToOpenAI(strings.NewReader(input), &sb)
	if err != nil {
		t.Fatalf("stream convert: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(sb.String()), "\n")
	if !strings.HasSuffix(sb.String(), "data: [DONE]\n\n") {
		t.Errorf("missing [DONE]")
	}
	var texts []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if !strings.HasPrefix(l, "data:") {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(l, "data:"))), &chunk); err != nil {
			if strings.Contains(l, "[DONE]") {
				continue
			}
			t.Fatalf("bad sse json %q: %v", l, err)
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			texts = append(texts, chunk.Choices[0].Delta.Content)
		}
	}
	if strings.Join(texts, "") != "你好" {
		t.Errorf("streamed text = %q, want 你好", strings.Join(texts, ""))
	}
}

// TestStreamGeminiSSEInput 验证 Gemini 以 SSE 格式返回时同样兼容。
func TestStreamGeminiSSEInput(t *testing.T) {
	input := "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Hi\"}]}}]}\n\n" +
		"data: {\"candidates\":[{\"content\":{\"role\":\"model\"},\"finishReason\":\"MAX_TOKENS\"}]}\n\n"
	var sb strings.Builder
	if err := StreamGeminiToOpenAI(strings.NewReader(input), &sb); err != nil {
		t.Fatalf("stream convert: %v", err)
	}
	if !strings.Contains(sb.String(), "\"content\":\"Hi\"") {
		t.Errorf("missing text: %q", sb.String())
	}
	if !strings.Contains(sb.String(), "\"finish_reason\":\"length\"") {
		t.Errorf("finish_reason mapping failed: %q", sb.String())
	}
}