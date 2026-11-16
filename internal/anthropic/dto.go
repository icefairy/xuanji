// Package anthropic 定义 Anthropic(Claude) Messages API 的请求、响应与流式事件 DTO。
package anthropic

// 流式事件类型。
const (
	EventTypeMessageStart      = "message_start"
	EventTypeContentBlockStart = "content_block_start"
	EventTypeContentBlockDelta = "content_block_delta"
	EventTypeContentBlockStop  = "content_block_stop"
	EventTypeMessageDelta      = "message_delta"
	EventTypeMessageStop       = "message_stop"
)

// 内容块类型。
const (
	ContentTypeText       = "text"
	ContentTypeImage      = "image"
	ContentTypeToolUse    = "tool_use"
	ContentTypeToolResult = "tool_result"
)

// ClaudeRequest 对应 Anthropic Messages API 的请求体（POST /v1/messages）。
type ClaudeRequest struct {
	Model         string            `json:"model"`
	System        any               `json:"system,omitempty"` // string 或 []ClaudeContentBlock
	Messages      []ClaudeMessage   `json:"messages,omitempty"`
	MaxTokens     *int              `json:"max_tokens,omitempty"`
	Temperature   *float64          `json:"temperature,omitempty"`
	TopP          *float64          `json:"top_p,omitempty"`
	TopK          *int              `json:"top_k,omitempty"`
	StopSequences []string          `json:"stop_sequences,omitempty"`
	Stream        *bool             `json:"stream,omitempty"`
	Tools         []ClaudeTool      `json:"tools,omitempty"`
	ToolChoice    *ClaudeToolChoice `json:"tool_choice,omitempty"`
}

// ClaudeMessage 是对话消息，Content 为字符串或 []ClaudeContentBlock。
type ClaudeMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// ClaudeContentBlock 是消息内容的组成块，按 Type 区分文本/图片/工具调用/工具结果。
type ClaudeContentBlock struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// image
	Source *ClaudeImageSource `json:"source,omitempty"`

	// tool_use
	ID    string `json:"id,omitempty"`
	Name  string `json:"name,omitempty"`
	Input any    `json:"input,omitempty"`

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   any    `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// ClaudeImageSource 是 image 内容块的来源。
type ClaudeImageSource struct {
	Type      string `json:"type"` // base64 / url
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// ClaudeTool 定义可提供给模型的工具。
type ClaudeTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

// ClaudeToolChoice 控制模型如何选择工具。
type ClaudeToolChoice struct {
	Type string `json:"type"` // auto / any / tool
	Name string `json:"name,omitempty"`
}

// ClaudeUsage 统计请求的 token 用量。
type ClaudeUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	OutputTokens             int `json:"output_tokens"`
}

// ClaudeResponse 对应 Anthropic Messages API 的非流式响应体。
type ClaudeResponse struct {
	ID           string               `json:"id,omitempty"`
	Type         string               `json:"type"` // message
	Role         string               `json:"role,omitempty"`
	Content      []ClaudeContentBlock `json:"content,omitempty"`
	Model        string               `json:"model,omitempty"`
	StopReason   string               `json:"stop_reason,omitempty"`
	StopSequence string               `json:"stop_sequence,omitempty"`
	Usage        *ClaudeUsage         `json:"usage,omitempty"`
}

// ClaudeStreamEvent 是 SSE 流式事件的公共前缀，通过 Type 判断具体事件。
type ClaudeStreamEvent struct {
	Type string `json:"type"`
}

// ClaudeStreamEventMessageStart 对应 message_start 事件，携带完整消息快照。
type ClaudeStreamEventMessageStart struct {
	Type    string         `json:"type"`
	Message ClaudeResponse `json:"message"`
}

// ClaudeStreamEventContentBlockStart 对应 content_block_start 事件。
type ClaudeStreamEventContentBlockStart struct {
	Type         string             `json:"type"`
	Index        int                `json:"index"`
	ContentBlock ClaudeContentBlock `json:"content_block"`
}

// ClaudeStreamEventContentBlockDelta 对应 content_block_delta 事件。
type ClaudeStreamEventContentBlockDelta struct {
	Type  string      `json:"type"`
	Index int         `json:"index"`
	Delta ClaudeDelta `json:"delta"`
}

// ClaudeStreamEventContentBlockStop 对应 content_block_stop 事件。
type ClaudeStreamEventContentBlockStop struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

// ClaudeStreamEventMessageDelta 对应 message_delta 事件。
type ClaudeStreamEventMessageDelta struct {
	Type  string       `json:"type"`
	Delta ClaudeDelta  `json:"delta"`
	Usage *ClaudeUsage `json:"usage,omitempty"`
}

// ClaudeStreamEventMessageStop 对应 message_stop 事件。
type ClaudeStreamEventMessageStop struct {
	Type string `json:"type"`
}

// ClaudeDelta 是流式增量内容，按 Type 区分 text_delta / input_json_delta / thinking_delta 等。
type ClaudeDelta struct {
	Type         string `json:"type"`
	Text         string `json:"text,omitempty"`
	PartialJSON  string `json:"partial_json,omitempty"`
	Thinking     string `json:"thinking,omitempty"`
	Signature    string `json:"signature,omitempty"`
	StopReason   string `json:"stop_reason,omitempty"`
	StopSequence string `json:"stop_sequence,omitempty"`
}
