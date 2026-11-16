package anthropic

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// 流式增量类型（Anthropic content_block_delta / message_delta）。
const (
	deltaTypeText      = "text_delta"
	deltaTypeInputJSON = "input_json_delta"
)

// OpenAI 请求/响应内部结构（仅用于协议转换，不对外暴露）。

// openAIChatRequest 对应 OpenAI chat/completions 请求体。
type openAIChatRequest struct {
	Model       string       `json:"model"`
	Messages    []any        `json:"messages"`
	MaxTokens   int          `json:"max_tokens"`
	Temperature *float64     `json:"temperature,omitempty"`
	TopP        *float64     `json:"top_p,omitempty"`
	Stream      *bool        `json:"stream,omitempty"`
	Stop        []string     `json:"stop,omitempty"`
	Tools       []openAITool `json:"tools,omitempty"`
	ToolChoice  any          `json:"tool_choice,omitempty"`
}

// openAIMessage 对应 OpenAI messages 数组里的单条消息。
type openAIMessage struct {
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

// openAIImageURL 是 OpenAI 图片内容块的 url 对象。
type openAIImageURL struct {
	URL string `json:"url"`
}

// openAIContentImage 是 OpenAI 图片内容块。
type openAIContentImage struct {
	Type     string         `json:"type"`
	ImageURL openAIImageURL `json:"image_url"`
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
	ID      string `json:"id"`
	Model   string `json:"model"`
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
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

// normalizeContent 把 any 类型的内容规范化为强类型。
// json.Unmarshal 到 any 时，content/system 数组会变成 []interface{} 而非
// []ClaudeContentBlock，导致 switch 类型断言全部落空（messages 变 null、
// system 丢失）——Claude Code 走 /v1/messages 时消息"消失"的根因
// （2026-08-02 修复）。
func normalizeContent(v any) any {
	switch val := v.(type) {
	case []any:
		b, err := json.Marshal(val)
		if err != nil {
			return v
		}
		var blocks []ClaudeContentBlock
		if err := json.Unmarshal(b, &blocks); err != nil {
			return v
		}
		return blocks
	default:
		return v
	}
}

// ClaudeRequestToOpenAI 把 Anthropic Messages 请求转换为 OpenAI chat/completions
// 请求体 JSON。system 拆成最前的 system role 消息；user 的 text/image 块转为
// 内容数组、tool_result 拆为 role:tool 消息；assistant 的 text 合并、tool_use 转为
// tool_calls；max_tokens 缺省 4096；tools 的 input_schema 作为 function.parameters。
func ClaudeRequestToOpenAI(req *ClaudeRequest) ([]byte, error) {
	var messages []any
	sys := normalizeContent(req.System)
	if s := claudeSystemToString(sys); s != "" {
		messages = append(messages, openAIMessage{Role: "system", Content: s})
	}

	for _, m := range req.Messages {
		content := normalizeContent(m.Content)
		switch m.Role {
		case "user":
			parts, tools := claudeUserContentToOpenAI(content)
			if len(parts) > 0 {
				messages = append(messages, openAIMessage{Role: "user", Content: parts})
			}
			messages = append(messages, tools...)
		case "assistant":
			nm := m
			nm.Content = content
			msg, err := claudeAssistantToOpenAI(nm)
			if err != nil {
				return nil, err
			}
			messages = append(messages, msg)
		default:
			// 防御：未知角色原样透传，避免破坏对话上下文
			messages = append(messages, openAIMessage{Role: m.Role, Content: content})
		}
	}

	maxTokens := 4096
	if req.MaxTokens != nil {
		maxTokens = *req.MaxTokens
	}

	reqBody := openAIChatRequest{
		Model:       req.Model,
		Messages:    messages,
		MaxTokens:   maxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      req.Stream,
		Stop:        req.StopSequences,
		ToolChoice:  claudeToolChoiceToOpenAI(req.ToolChoice),
	}
	if len(req.Tools) > 0 {
		tools := make([]openAITool, 0, len(req.Tools))
		for _, t := range req.Tools {
			params := t.InputSchema
			if params == nil {
				params = map[string]any{}
			}
			tools = append(tools, openAITool{
				Type: "function",
				Function: openAIToolFunction{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  params,
				},
			})
		}
		reqBody.Tools = tools
	}

	return json.Marshal(reqBody)
}

// claudeSystemToString 把 system 字段（string 或文本块数组）转为纯字符串。
func claudeSystemToString(system any) string {
	switch v := system.(type) {
	case string:
		return v
	case []ClaudeContentBlock:
		var b strings.Builder
		for _, blk := range v {
			if blk.Type == ContentTypeText {
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	}
	return ""
}

// claudeUserContentToOpenAI 把 user 消息内容转为 OpenAI 内容数组与拆出的 tool 消息列表。
func claudeUserContentToOpenAI(content any) ([]any, []any) {
	var parts []any
	var tools []any
	switch c := content.(type) {
	case string:
		parts = append(parts, openAIContentText{Type: "text", Text: c})
	case []ClaudeContentBlock:
		for _, blk := range c {
			switch blk.Type {
			case ContentTypeText:
				parts = append(parts, openAIContentText{Type: "text", Text: blk.Text})
			case ContentTypeImage:
				if url := claudeImageDataURL(blk.Source); url != "" {
					parts = append(parts, openAIContentImage{
						Type:     "image_url",
						ImageURL: openAIImageURL{URL: url},
					})
				}
			case ContentTypeToolResult:
				tools = append(tools, openAIMessage{
					Role:       "tool",
					ToolCallID: blk.ToolUseID,
					Content:    claudeContentToString(blk.Content),
				})
			}
		}
	default:
		if s, ok := content.(string); ok {
			parts = append(parts, openAIContentText{Type: "text", Text: s})
		}
	}
	return parts, tools
}

// claudeImageDataURL 把 image 块 source 转为 data URL；url 类型原样返回。
func claudeImageDataURL(src *ClaudeImageSource) string {
	if src == nil {
		return ""
	}
	switch src.Type {
	case "base64":
		mt := src.MediaType
		if mt == "" {
			mt = "image/png"
		}
		return "data:" + mt + ";base64," + src.Data
	default:
		return src.URL
	}
}

// claudeAssistantToOpenAI 把 assistant 消息转为 OpenAI 消息，text 合并、tool_use 转 tool_calls。
func claudeAssistantToOpenAI(m ClaudeMessage) (openAIMessage, error) {
	msg := openAIMessage{Role: "assistant"}
	switch c := m.Content.(type) {
	case string:
		msg.Content = c
	case []ClaudeContentBlock:
		var textParts []string
		var toolCalls []openAIToolCall
		for _, blk := range c {
			switch blk.Type {
			case ContentTypeText:
				textParts = append(textParts, blk.Text)
			case ContentTypeToolUse:
				var args string
				if blk.Input == nil {
					args = "{}"
				} else {
					b, err := json.Marshal(blk.Input)
					if err != nil {
						return msg, fmt.Errorf("marshal tool_use input: %w", err)
					}
					args = string(b)
				}
				toolCalls = append(toolCalls, openAIToolCall{
					ID:   blk.ID,
					Type: "function",
					Function: openAIToolCallFunction{
						Name:      blk.Name,
						Arguments: args,
					},
				})
			}
		}
		if len(textParts) > 0 {
			msg.Content = strings.Join(textParts, "")
		}
		if len(toolCalls) > 0 {
			msg.ToolCalls = toolCalls
		}
	default:
		msg.Content = c
	}
	return msg, nil
}

// claudeContentToString 把 tool_result 的 content（字符串或块数组）转为纯字符串。
func claudeContentToString(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []ClaudeContentBlock:
		var b strings.Builder
		for _, blk := range v {
			if blk.Type == ContentTypeText {
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	}
	return ""
}

// claudeToolChoiceToOpenAI 把 Claude 的 tool_choice 映射为 OpenAI 形式。
func claudeToolChoiceToOpenAI(tc *ClaudeToolChoice) any {
	if tc == nil {
		return nil
	}
	switch tc.Type {
	case "any":
		return "required"
	case "tool":
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": tc.Name},
		}
	default: // "auto" 及未知类型
		return "auto"
	}
}

// OpenAIResponseToClaude 把 OpenAI chat/completions 非流式响应转换为 Anthropic
// Messages 响应。id 加 msg_ 前缀，content 转 text 块，tool_calls 转 tool_use 块，
// finish_reason 映射，usage 字段映射。
func OpenAIResponseToClaude(openAIJSON []byte, model string) (*ClaudeResponse, error) {
	var resp openAICompletion
	if err := json.Unmarshal(openAIJSON, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal openai response: %w", err)
	}

	out := &ClaudeResponse{
		ID:    "msg_" + resp.ID,
		Type:  "message",
		Role:  "assistant",
		Model: model,
	}

	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		var content []ClaudeContentBlock
		if choice.Message.Content != nil && *choice.Message.Content != "" {
			content = append(content, ClaudeContentBlock{Type: ContentTypeText, Text: *choice.Message.Content})
		}
		for _, tc := range choice.Message.ToolCalls {
			var input any = map[string]any{}
			if s := strings.TrimSpace(tc.Function.Arguments); s != "" {
				if err := json.Unmarshal([]byte(s), &input); err != nil {
					input = map[string]any{}
				}
			}
			content = append(content, ClaudeContentBlock{
				Type:  ContentTypeToolUse,
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: input,
			})
		}
		if len(content) > 0 {
			out.Content = content
		}
		if stop, ok := mapFinishReason(choice.FinishReason); ok {
			out.StopReason = stop
		}
	}

	out.Usage = &ClaudeUsage{
		InputTokens:  resp.Usage.PromptTokens,
		OutputTokens: resp.Usage.CompletionTokens,
	}
	return out, nil
}

// mapFinishReason 把 OpenAI 的 finish_reason 映射为 Anthropic 的 stop_reason。
func mapFinishReason(fr string) (string, bool) {
	switch fr {
	case "stop":
		return "end_turn", true
	case "length":
		return "max_tokens", true
	case "tool_calls":
		return "tool_use", true
	}
	return "", false
}

// sseState 维护 OpenAI SSE → Claude SSE 转换过程中的流式状态。
type sseState struct {
	w         io.Writer
	started   bool // 是否已发送 message_start
	finished  bool // 是否已发送 message_delta
	respID    string
	model     string
	blocks    []bool                  // 每个 Anthropic content block 是否已 start 未 stop
	nextBlock int                     // 下一个可分配的 content block index
	textBlock int                     // text 块的 index，-1 表示尚未开始
	toolState map[int]*toolBlockState // OpenAI tool index → 工具块状态
}

// toolBlockState 记录一个工具调用块的转换状态。
type toolBlockState struct {
	block int
	id    string
	name  string
}

// newSSEState 创建流式转换状态。
func newSSEState(w io.Writer) *sseState {
	return &sseState{w: w, textBlock: -1, toolState: make(map[int]*toolBlockState)}
}

// allocBlock 分配下一个 content block index 并标记为已开始。
func (s *sseState) allocBlock() int {
	idx := s.nextBlock
	s.nextBlock++
	for len(s.blocks) <= idx {
		s.blocks = append(s.blocks, false)
	}
	s.blocks[idx] = true
	return idx
}

// ensureMessageStart 在收到第一个有效 chunk 时发送 message_start 事件。
func (s *sseState) ensureMessageStart() error {
	if s.started {
		return nil
	}
	s.started = true
	id := "msg_" + s.respID
	if s.respID == "" {
		id = "msg_stream"
	}
	return writeSSE(s.w, EventTypeMessageStart, ClaudeStreamEventMessageStart{
		Type: EventTypeMessageStart,
		Message: ClaudeResponse{
			ID:    id,
			Type:  "message",
			Role:  "assistant",
			Model: s.model,
			Usage: &ClaudeUsage{},
		},
	})
}

// stopAllBlocks 对已开始未停止的 content block 依次发送 content_block_stop。
func (s *sseState) stopAllBlocks() error {
	for i, started := range s.blocks {
		if !started {
			continue
		}
		if err := writeSSE(s.w, EventTypeContentBlockStop, ClaudeStreamEventContentBlockStop{
			Type:  EventTypeContentBlockStop,
			Index: i,
		}); err != nil {
			return err
		}
		s.blocks[i] = false
	}
	return nil
}

// emitMessageDelta 发送 message_delta 事件；stop 为空时 stop_reason 置 null。
func (s *sseState) emitMessageDelta(stop string) error {
	delta := map[string]any{"stop_reason": stop, "stop_sequence": nil}
	return writeSSE(s.w, EventTypeMessageDelta, map[string]any{
		"type":  EventTypeMessageDelta,
		"delta": delta,
	})
}

// finishStream 补全流结尾：停掉所有块、补发 message_delta，最后发 message_stop。
func (s *sseState) finishStream() error {
	if err := s.stopAllBlocks(); err != nil {
		return err
	}
	if !s.finished {
		if err := s.emitMessageDelta("end_turn"); err != nil {
			return err
		}
	}
	return writeSSE(s.w, EventTypeMessageStop, ClaudeStreamEventMessageStop{Type: EventTypeMessageStop})
}

// OpenAISSEToClaudeSSE 逐行读取 OpenAI 的 SSE 流（data: 前缀），转换为 Anthropic
// 的 SSE 事件流写入 w。每个事件格式为 "event: X\ndata: {...}\n\n"。
func OpenAISSEToClaudeSSE(scanner *bufio.Scanner, w io.Writer) error {
	s := newSSEState(w)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))

		if payload == "[DONE]" {
			if s.started {
				if err := s.finishStream(); err != nil {
					return err
				}
			}
			flushWriter(w)
			return nil
		}

		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return fmt.Errorf("unmarshal openai chunk: %w", err)
		}
		if len(chunk.Choices) == 0 {
			continue // 仅含 usage 的 chunk，跳过
		}
		if s.respID == "" {
			s.respID = chunk.ID
		}
		if s.model == "" {
			s.model = chunk.Model
		}
		if err := s.ensureMessageStart(); err != nil {
			return err
		}

		choice := chunk.Choices[0]

		// 文本增量
		if choice.Delta.Content != "" {
			if s.textBlock < 0 {
				s.textBlock = s.allocBlock()
				if err := writeSSE(s.w, EventTypeContentBlockStart, ClaudeStreamEventContentBlockStart{
					Type:         EventTypeContentBlockStart,
					Index:        s.textBlock,
					ContentBlock: ClaudeContentBlock{Type: ContentTypeText, Text: ""},
				}); err != nil {
					return err
				}
			}
			if err := writeSSE(s.w, EventTypeContentBlockDelta, ClaudeStreamEventContentBlockDelta{
				Type:  EventTypeContentBlockDelta,
				Index: s.textBlock,
				Delta: ClaudeDelta{Type: deltaTypeText, Text: choice.Delta.Content},
			}); err != nil {
				return err
			}
		}

		// 工具调用增量
		for _, tc := range choice.Delta.ToolCalls {
			st, ok := s.toolState[tc.Index]
			if !ok {
				st = &toolBlockState{block: s.allocBlock()}
				s.toolState[tc.Index] = st
				if err := writeSSE(s.w, EventTypeContentBlockStart, ClaudeStreamEventContentBlockStart{
					Type:  EventTypeContentBlockStart,
					Index: st.block,
					ContentBlock: ClaudeContentBlock{
						Type:  ContentTypeToolUse,
						ID:    st.id,
						Name:  st.name,
						Input: map[string]any{},
					},
				}); err != nil {
					return err
				}
			}
			if tc.ID != "" {
				st.id = tc.ID
			}
			if tc.Function.Name != "" {
				st.name = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				if err := writeSSE(s.w, EventTypeContentBlockDelta, ClaudeStreamEventContentBlockDelta{
					Type:  EventTypeContentBlockDelta,
					Index: st.block,
					Delta: ClaudeDelta{Type: deltaTypeInputJSON, PartialJSON: tc.Function.Arguments},
				}); err != nil {
					return err
				}
			}
		}

		// 结束原因
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			if err := s.stopAllBlocks(); err != nil {
				return err
			}
			stop, _ := mapFinishReason(*choice.FinishReason)
			if err := s.emitMessageDelta(stop); err != nil {
				return err
			}
			s.finished = true
		}

		flushWriter(w)
	}

	if err := scanner.Err(); err != nil {
		return err
	}
	// 上游 EOF 未发 [DONE]：补全结束事件
	if s.started {
		if err := s.finishStream(); err != nil {
			return err
		}
		flushWriter(w)
	}
	return nil
}

// writeSSE 以 "event: X\ndata: {...}\n\n" 格式写一个事件。
func writeSSE(w io.Writer, event string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	return err
}

// flushWriter 尽力冲刷 writer，兼容 bufio.Writer 与 http.ResponseWriter。
func flushWriter(w io.Writer) {
	type flushErr interface{ Flush() error }
	if f, ok := w.(flushErr); ok {
		_ = f.Flush()
		return
	}
	type flusher interface{ Flush() }
	if f, ok := w.(flusher); ok {
		f.Flush()
	}
}

// ClaudeError 生成 Anthropic 标准错误响应体，错误类型按 HTTP 状态码映射。
func ClaudeError(status int, msg string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    anthropicErrorType(status),
			"message": msg,
		},
	})
	return b
}

// anthropicErrorType 把 HTTP 状态码映射为 Anthropic 错误类型。
func anthropicErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable:
		return "overloaded_error"
	default:
		return "api_error"
	}
}
