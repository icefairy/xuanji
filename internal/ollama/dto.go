// Package ollama 定义 Ollama 原生协议的 DTO 与 OpenAI ↔ Ollama 协议转换。
package ollama

// OllamaChatRequest 对应 Ollama POST /api/chat 的请求体。
type OllamaChatRequest struct {
	Model     string              `json:"model"`
	Messages  []OllamaChatMessage `json:"messages,omitempty"`
	Stream    *bool               `json:"stream,omitempty"`
	Tools     []OllamaTool        `json:"tools,omitempty"`
	Format    any                 `json:"format,omitempty"` // "json" 或 JSON schema 对象
	Options   map[string]any      `json:"options,omitempty"`
	KeepAlive any                 `json:"keep_alive,omitempty"` // 时长字符串或秒数
}

// OllamaChatMessage 是 /api/chat 的对话消息。
type OllamaChatMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	Thinking  string           `json:"thinking,omitempty"`
	ToolCalls []OllamaToolCall `json:"tool_calls,omitempty"`
	Images    []string         `json:"images,omitempty"`
}

// OllamaToolCall 是消息里的工具调用，Arguments 为已解析的 JSON 对象。
type OllamaToolCall struct {
	Function OllamaToolCallFunction `json:"function"`
}

// OllamaToolCallFunction 是工具调用的函数体。
type OllamaToolCallFunction struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Arguments   any    `json:"arguments,omitempty"`
}

// OllamaTool 定义 /api/chat 请求里可提供给模型的工具。
type OllamaTool struct {
	Type     string             `json:"type"`
	Function OllamaToolFunction `json:"function"`
}

// OllamaToolFunction 是工具的函数描述，Parameters 与 OpenAI 的 function.parameters 同构。
type OllamaToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

// OllamaChatResponse 对应 Ollama POST /api/chat 的非流式响应体。
type OllamaChatResponse struct {
	Model           string            `json:"model"`
	Message         OllamaChatMessage `json:"message"`
	Done            bool              `json:"done"`
	DoneReason      string            `json:"done_reason,omitempty"`
	PromptEvalCount int               `json:"prompt_eval_count,omitempty"`
	EvalCount       int               `json:"eval_count,omitempty"`
}

// OllamaGenerateRequest 对应 Ollama POST /api/generate 的请求体。
type OllamaGenerateRequest struct {
	Model   string         `json:"model"`
	Prompt  string         `json:"prompt,omitempty"`
	System  string         `json:"system,omitempty"`
	Stream  *bool          `json:"stream,omitempty"`
	Options map[string]any `json:"options,omitempty"`
	Raw     bool           `json:"raw,omitempty"`
}

// OllamaGenerateResponse 对应 Ollama POST /api/generate 的响应体。
type OllamaGenerateResponse struct {
	Model           string `json:"model"`
	Response        string `json:"response,omitempty"`
	Thinking        string `json:"thinking,omitempty"`
	Done            bool   `json:"done"`
	DoneReason      string `json:"done_reason,omitempty"`
	PromptEvalCount int    `json:"prompt_eval_count,omitempty"`
	EvalCount       int    `json:"eval_count,omitempty"`
}

// OllamaEmbedRequest 对应 Ollama POST /api/embed 的请求体。
type OllamaEmbedRequest struct {
	Model    string         `json:"model"`
	Input    any            `json:"input"` // string 或 []string
	Truncate *bool          `json:"truncate,omitempty"`
	Options  map[string]any `json:"options,omitempty"`
}

// OllamaEmbedResponse 对应 Ollama POST /api/embed 的响应体。
type OllamaEmbedResponse struct {
	Model           string      `json:"model"`
	Embeddings      [][]float64 `json:"embeddings"`
	PromptEvalCount int         `json:"prompt_eval_count,omitempty"`
}
