package gemini

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// ── OpenAI 内部结构（仅用于协议转换，不对外暴露）──────────────────────

// openAIChatRequest 对应 OpenAI chat/completions 请求体。
type openAIChatRequest struct {
	Model       string       `json:"model"`
	Messages    []openAIMsg  `json:"messages"`
	MaxTokens   int          `json:"max_tokens"`
	Temperature *float64     `json:"temperature,omitempty"`
	TopP        *float64     `json:"top_p,omitempty"`
	Stream      *bool        `json:"stream,omitempty"`
	Stop        []string     `json:"stop,omitempty"`
	Tools       []openAITool `json:"tools,omitempty"`
	ToolChoice  any          `json:"tool_choice,omitempty"`
}

// openAIMsg 是对话消息，Content 为字符串或内容块数组。
type openAIMsg struct {
	Role       string           `json:"role"`
	Content    any              `json:"content,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
}

// openAIContentText 是 OpenAI 文本内容块。
type openAIContentText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// openAIContentImage 是 OpenAI 图片内容块。
type openAIContentImage struct {
	Type     string         `json:"type"`
	ImageURL openAIImageURL `json:"image_url"`
}

// openAIImageURL 是图片 url 对象。
type openAIImageURL struct {
	URL string `json:"url"`
}

// openAIToolCall 对应 OpenAI assistant 消息里的工具调用。
type openAIToolCall struct {
	ID       string                 `json:"id,omitempty"`
	Type     string                 `json:"type,omitempty"`
	Function openAIToolCallFunction `json:"function"`
}

// openAIToolCallFunction 是工具调用的函数体。
type openAIToolCallFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// openAITool 对应 OpenAI 请求体的 tools 数组元素。
type openAITool struct {
	Type     string             `json:"type"`
	Function openAIToolFunction `json:"function"`
}

// openAIToolFunction 是工具的函数描述。
type openAIToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

// openAICompletion 对应 OpenAI chat/completions 非流式响应体。
type openAICompletion struct {
	ID      string `json:"id,omitempty"`
	Model   string `json:"model,omitempty"`
	Choices []struct {
		Message struct {
			Role      string  `json:"role"`
			Content   *string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// openAIStreamChunk 对应 OpenAI chat/completions 流式响应里的单个 chunk。
type openAIStreamChunk struct {
	ID      string              `json:"id,omitempty"`
	Object  string              `json:"object,omitempty"`
	Model   string              `json:"model,omitempty"`
	Choices []openAIStreamChoice `json:"choices"`
}

// openAIStreamChoice 是流式 chunk 的单个 choice。
type openAIStreamChoice struct {
	Index        int                `json:"index"`
	Delta        openAIStreamDelta  `json:"delta"`
	FinishReason *string            `json:"finish_reason"`
}

// openAIStreamDelta 是流式增量。
type openAIStreamDelta struct {
	Role      string                `json:"role,omitempty"`
	Content   string                `json:"content"`
	ToolCalls []openAIStreamToolCall `json:"tool_calls"`
}

// openAIStreamToolCall 是流式工具调用增量。
type openAIStreamToolCall struct {
	Index    int                   `json:"index"`
	ID       string                `json:"id"`
	Type     string                `json:"type"`
	Function openAIToolCallFunction `json:"function"`
}

// toolCallAccumulator 累积流式工具调用参数（OpenAI 的 arguments 是分片 JSON 字符串，
// Gemini 的 functionCall.args 是完整对象，需要先拼接再解析）。
type toolCallAccumulator struct {
	id   string
	name string
	args strings.Builder
}

// ── OpenAI → Gemini（请求）──────────────────────────────────────────

// OpenAIChatToGemini 把 OpenAI chat/completions 请求转换为 Gemini
// generateContent 请求体 JSON。system 拆为 systemInstruction；user 的
// 文本/图片块转为 parts（inlineData）；assistant 的 tool_calls 转为
// functionCall parts；tool 消息转为 functionResponse parts。
// 转换后的 model 字段为 upModel（由 handler 传入，替换为上游真实模型名）。
func OpenAIChatToGemini(openAIReq []byte, upModel string) ([]byte, error) {
	var oreq openAIChatRequest
	if err := json.Unmarshal(openAIReq, &oreq); err != nil {
		return nil, fmt.Errorf("invalid openai request: %w", err)
	}

	greq := GeminiRequest{Model: upModel}
	var sysParts []GeminiPart

	for _, m := range oreq.Messages {
		switch m.Role {
		case "system":
			if s := openAIContentToString(m.Content); s != "" {
				sysParts = append(sysParts, GeminiPart{Text: s})
			}
		case "user":
			c := openAIPartsToGemini(m.Content)
			if c != nil {
				greq.Contents = append(greq.Contents, GeminiContent{Role: RoleUser, Parts: c})
			}
		case "assistant":
			c := openAIAssistantToGemini(m)
			if c != nil {
				greq.Contents = append(greq.Contents, *c)
			}
		case "tool":
			// OpenAI tool 消息 → user 消息里的 functionResponse part
			if m.ToolCallID != "" {
				var data map[string]any
				if s, ok := m.Content.(string); ok {
					_ = json.Unmarshal([]byte(s), &data)
				}
				if data == nil {
					data = map[string]any{}
				}
				greq.Contents = append(greq.Contents, GeminiContent{
					Role: RoleUser,
					Parts: []GeminiPart{{
						FunctionResponse: &GeminiFunctionResponse{
							Name:     m.ToolCallID,
							Response: data,
						},
					}},
				})
			}
		}
	}

	if len(sysParts) > 0 {
		greq.SystemInstruction = &GeminiContent{Parts: sysParts}
	}

	// tools → functionDeclarations
	if len(oreq.Tools) > 0 {
		var decls []GeminiFunctionDecl
		for _, t := range oreq.Tools {
			for _, fn := range []openAIToolFunction{t.Function} {
				params := fn.Parameters
				if params == nil {
					params = map[string]any{}
				}
				decls = append(decls, GeminiFunctionDecl{
					Name:        fn.Name,
					Description: fn.Description,
					Parameters:  params,
				})
			}
		}
		if len(decls) > 0 {
			greq.Tools = []GeminiTool{{FunctionDeclarations: decls}}
		}
	}

	// tool_choice → functionCallingConfig
	if tc := openAIToolChoiceToGemini(oreq.ToolChoice); tc != nil {
		greq.ToolConfig = &GeminiToolConfig{FunctionCallingConfig: tc}
	}

	// 生成参数
	if oreq.MaxTokens > 0 || oreq.Temperature != nil || oreq.TopP != nil || len(oreq.Stop) > 0 {
		gc := &GeminiGenerationConfig{}
		if oreq.MaxTokens > 0 {
			gc.MaxOutputTokens = &oreq.MaxTokens
		}
		gc.Temperature = oreq.Temperature
		gc.TopP = oreq.TopP
		gc.StopSequences = oreq.Stop
		greq.GenerationConfig = gc
	}

	return json.Marshal(greq)
}

// openAIContentToString 把 OpenAI 消息 content（字符串或块数组）转为纯文本。
func openAIContentToString(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				if text, ok := m["text"].(string); ok {
					b.WriteString(text)
				}
			}
		}
		return b.String()
	}
	return ""
}

// openAIPartsToGemini 把 user 消息 content 转成 Gemini parts（文本/图片）。
func openAIPartsToGemini(content any) []GeminiPart {
	var parts []GeminiPart
	switch v := content.(type) {
	case string:
		if v != "" {
			parts = append(parts, GeminiPart{Text: v})
		}
	case []any:
		for _, item := range v {
			switch b := item.(type) {
			case string:
				parts = append(parts, GeminiPart{Text: b})
			case map[string]any:
				switch b["type"] {
				case "text":
					if s, ok := b["text"].(string); ok && s != "" {
						parts = append(parts, GeminiPart{Text: s})
					}
				case "image_url":
					iu, _ := b["image_url"].(map[string]any)
					u, _ := iu["url"].(string)
					if u == "" {
						continue
					}
					if strings.HasPrefix(u, "data:") {
						// data URI 拆成 mimeType + base64
						if comma := strings.IndexByte(u, ','); comma > 0 {
							meta := u[5:comma] // data:image/png;base64,...
							midx := strings.Index(meta, ";")
							mime := meta
							if midx > 0 {
								mime = meta[:midx]
							}
							parts = append(parts, GeminiPart{
								InlineData: &GeminiBlob{MimeType: mime, Data: u[comma+1:]},
							})
						}
					} else {
						// 远程 URL：Gemini 不支持直接 URL，跳过
						parts = append(parts, GeminiPart{Text: "[image omitted: " + u + "]"})
					}
				}
			}
		}
	}
	return parts
}

// openAIAssistantToGemini 把 assistant 消息转为 Gemini model 内容。
func openAIAssistantToGemini(m openAIMsg) *GeminiContent {
	c := &GeminiContent{Role: RoleModel}
	if m.Content != nil {
		switch v := m.Content.(type) {
		case string:
			if v != "" {
				c.Parts = append(c.Parts, GeminiPart{Text: v})
			}
		case []any:
			for _, item := range v {
				if mm, ok := item.(map[string]any); ok {
					if s, ok := mm["text"].(string); ok && s != "" {
						c.Parts = append(c.Parts, GeminiPart{Text: s})
					}
				}
			}
		}
	}
	for _, tc := range m.ToolCalls {
		var args map[string]any
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		c.Parts = append(c.Parts, GeminiPart{
			FunctionCall: &GeminiFunctionCall{Name: tc.Function.Name, Args: args},
		})
	}
	if len(c.Parts) == 0 {
		return nil
	}
	return c
}

// openAIToolChoiceToGemini 把 OpenAI tool_choice 映射为 Gemini functionCallingConfig。
func openAIToolChoiceToGemini(tc any) *GeminiFunctionCallingConfig {
	switch v := tc.(type) {
	case string:
		switch v {
		case "none":
			return &GeminiFunctionCallingConfig{Mode: "NONE"}
		case "auto":
			return &GeminiFunctionCallingConfig{Mode: "AUTO"}
		}
	case map[string]any:
		if t, ok := v["type"].(string); ok && t == "function" {
			if fn, ok := v["function"].(map[string]any); ok {
				if name, ok := fn["name"].(string); ok && name != "" {
					return &GeminiFunctionCallingConfig{
						Mode:                 "ANY",
						AllowedFunctionNames: []string{name},
					}
				}
			}
		}
	}
	return nil
}

// ── Gemini → OpenAI（非流式响应）────────────────────────────────────

// GeminiResponseToOpenAI 把 Gemini generateContent 响应转换为 OpenAI
// chat/completions 响应体 JSON。
func GeminiResponseToOpenAI(data []byte, model string) ([]byte, error) {
	var gresp GeminiResponse
	if err := json.Unmarshal(data, &gresp); err != nil {
		return nil, fmt.Errorf("invalid gemini response: %w", err)
	}

	resp := openAICompletion{
		ID:    "chatcmpl-gemini",
		Model: model,
	}
	if len(gresp.Candidates) > 0 {
		cand := gresp.Candidates[0]
		choice := resp.Choices[:0]
		_ = choice
		resp.Choices = append(resp.Choices, struct {
			Message struct {
				Role      string  `json:"role"`
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		}{
			FinishReason: geminiFinishToOpenAI(cand.FinishReason),
		})
		if cand.Content != nil {
			text := ""
			for _, p := range cand.Content.Parts {
				text += p.Text
				if p.FunctionCall != nil {
					args, _ := json.Marshal(p.FunctionCall.Args)
					argsS := string(args)
					resp.Choices[0].Message.ToolCalls = append(resp.Choices[0].Message.ToolCalls, struct {
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					}{
						ID:   p.FunctionCall.Name,
						Type: "function",
						Function: struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						}{Name: p.FunctionCall.Name, Arguments: argsS},
					})
				}
			}
			resp.Choices[0].Message.Role = "assistant"
			if text != "" || len(resp.Choices[0].Message.ToolCalls) > 0 {
				resp.Choices[0].Message.Content = &text
			}
		}
	}
	if gresp.UsageMetadata != nil {
		resp.Usage.PromptTokens = gresp.UsageMetadata.PromptTokenCount
		resp.Usage.CompletionTokens = gresp.UsageMetadata.CandidatesTokenCount
	}
	return json.Marshal(resp)
}

// geminiFinishToOpenAI 把 Gemini finishReason 映射为 OpenAI finish_reason。
func geminiFinishToOpenAI(finish string) string {
	switch finish {
	case FinishReasonMaxTokens:
		return "length"
	case FinishReasonSafety, FinishReasonRecitation:
		return "content_filter"
	default:
		return "stop"
	}
}

// ── Gemini 流 → OpenAI SSE（流式）────────────────────────────────────

// StreamGeminiToOpenAI 把 Gemini streamGenerateContent 响应流转换为 OpenAI
// SSE 事件流写回客户端。兼容两种上游格式：SSE（data: 前缀）与逐行 JSON。
func StreamGeminiToOpenAI(r io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	var accs []*toolCallAccumulator
	var lastText string
	started := false

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		for _, prefix := range []string{"data:", "data: "} {
			line = strings.TrimPrefix(line, prefix)
		}
		line = strings.TrimSpace(line)
		if line == "" || line == "[DONE]" {
			continue
		}
		var chunk struct {
			Candidates    []GeminiCandidate   `json:"candidates"`
			UsageMetadata *GeminiUsageMetadata `json:"usageMetadata,omitempty"`
		}
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			continue // 跳过非 JSON 行（如 keepalive）
		}
		if len(chunk.Candidates) == 0 {
			continue
		}
		cand := chunk.Candidates[0]
		out := openAIStreamChunk{ID: "chatcmpl-gemini", Object: "chat.completion.chunk", Model: ""}
		out.Choices = append(out.Choices, openAIStreamChoice{Index: 0})

		// 首个 chunk 带 role
		if !started {
			out.Choices[0].Delta.Role = "assistant"
			started = true
		}

		delta := &out.Choices[0].Delta
		if cand.Content != nil {
			for _, p := range cand.Content.Parts {
				if p.Text != "" {
					delta.Content += p.Text
				}
				if p.FunctionCall != nil {
					// 增量 functionCall：按 name 累积参数，产生 tool_calls 增量
					acc := findOrCreateAcc(accs, p.FunctionCall.Name)
					argsJSON, _ := json.Marshal(p.FunctionCall.Args)
					acc.args.WriteString(string(argsJSON))
					idx := indexOfAcc(accs, acc)
					delta.ToolCalls = append(delta.ToolCalls, openAIStreamToolCall{
						Index: idx,
						ID:    acc.id,
						Type:  "function",
						Function: openAIToolCallFunction{Name: acc.name, Arguments: acc.args.String()},
					})
				}
			}
		}
		if cand.FinishReason != "" && cand.FinishReason != lastText {
			fr := geminiFinishToOpenAI(cand.FinishReason)
			out.Choices[0].FinishReason = &fr
			lastText = cand.FinishReason
		}

		if delta.Content == "" && len(delta.ToolCalls) == 0 && out.Choices[0].FinishReason == nil {
			lastText = ""
			continue
		}
		if err := writeOpenAISSE(w, out); err != nil {
			return err
		}
		lastText = ""
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	// 结束标记
	_, err := fmt.Fprint(w, "data: [DONE]\n\n")
	return err
}

func findOrCreateAcc(accs []*toolCallAccumulator, name string) *toolCallAccumulator {
	for _, a := range accs {
		if a.name == name {
			return a
		}
	}
	acc := &toolCallAccumulator{id: "call_" + name, name: name}
	accs = append(accs, acc)
	return acc
}

func indexOfAcc(accs []*toolCallAccumulator, target *toolCallAccumulator) int {
	for i, a := range accs {
		if a == target {
			return i
		}
	}
	return 0
}

// writeOpenAISSE 以 SSE data: 行写一个 JSON 对象。
func writeOpenAISSE(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", bytes.TrimSpace(data))
	return err
}

// ── 错误格式 ─────────────────────────────────────────────────────────

// GeminiErrorJSON 以 Gemini API 错误格式生成响应体（供上游错误透传）。
func GeminiErrorJSON(status int, msg string) []byte {
	statusText := map[int]string{
		400: "INVALID_ARGUMENT",
		401: "UNAUTHENTICATED",
		403: "PERMISSION_DENIED",
		404: "NOT_FOUND",
		429: "RESOURCE_EXHAUSTED",
		500: "INTERNAL",
		502: "UNAVAILABLE",
		503: "UNAVAILABLE",
	}[status]
	if statusText == "" {
		statusText = "INTERNAL"
	}
	b, _ := json.Marshal(GeminiError{Error: GeminiErrorDetail{
		Code:    status,
		Message: msg,
		Status:  statusText,
	}})
	return b
}