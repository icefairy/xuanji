// Package ollama 定义 Ollama 原生协议的 DTO 与 OpenAI ↔ Ollama 协议转换。
package ollama

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// OpenAI 请求/响应内部结构（仅用于协议转换，不对外暴露）。

// openAIChatRequest 对应 OpenAI chat/completions 请求体。
type openAIChatRequest struct {
	Model       string              `json:"model"`
	Messages    []openAIChatMessage `json:"messages"`
	MaxTokens   *int                `json:"max_tokens"`
	Temperature *float64            `json:"temperature"`
	Stream      *bool               `json:"stream"`
	Tools       []openAITool        `json:"tools"`
}

// openAIChatMessage 对应 OpenAI messages 数组里的单条消息。
type openAIChatMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content"` // string 或内容块数组
	ToolCallID string           `json:"tool_call_id"`
	ToolCalls  []openAIToolCall `json:"tool_calls"`
}

// openAIContentBlock 是 OpenAI 消息内容数组里的元素。
type openAIContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL struct {
		URL string `json:"url"`
	} `json:"image_url"`
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

// openAIToolCall 对应 OpenAI assistant 消息或响应里的工具调用。
type openAIToolCall struct {
	ID       string                 `json:"id,omitempty"`
	Type     string                 `json:"type,omitempty"`
	Function openAIToolCallFunction `json:"function"`
}

// openAIToolCallFunction 是工具调用的函数体，Arguments 为 JSON 字符串。
type openAIToolCallFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// openAICompletion 对应 OpenAI chat/completions 非流式响应体。
type openAICompletion struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
	Usage   openAIUsage    `json:"usage"`
}

// openAIChoice 是响应的单个选择。
type openAIChoice struct {
	Index        int                   `json:"index"`
	Message      openAIResponseMessage `json:"message"`
	FinishReason string                `json:"finish_reason"`
}

// openAIResponseMessage 是响应里的 assistant 消息。
type openAIResponseMessage struct {
	Role      string           `json:"role"`
	Content   *string          `json:"content"`
	ToolCalls []openAIToolCall `json:"tool_calls,omitempty"`
}

// openAIUsage 统计 token 用量。
type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// openAIStreamChunk 对应 OpenAI chat/completions 流式响应里的单个 chunk。
type openAIStreamChunk struct {
	ID      string               `json:"id"`
	Object  string               `json:"object"`
	Created int64                `json:"created"`
	Model   string               `json:"model"`
	Choices []openAIStreamChoice `json:"choices"`
}

// openAIStreamChoice 是流式 chunk 的单个选择。
type openAIStreamChoice struct {
	Index        int               `json:"index"`
	Delta        openAIStreamDelta `json:"delta"`
	FinishReason *string           `json:"finish_reason"`
}

// openAIStreamDelta 是流式增量内容。
type openAIStreamDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// openAIEmbeddingResponse 对应 OpenAI /v1/embeddings 非流式响应体。
type openAIEmbeddingResponse struct {
	Object string            `json:"object"`
	Data   []openAIEmbedding `json:"data"`
	Model  string            `json:"model"`
	Usage  openAIEmbedUsage  `json:"usage"`
}

// openAIEmbedding 是单条向量。
type openAIEmbedding struct {
	Object    string    `json:"object"`
	Embedding []float64 `json:"embedding"`
	Index     int       `json:"index"`
}

// openAIEmbedUsage 统计嵌入请求的 token 用量。
type openAIEmbedUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// OpenAIChatToOllamaChat 把 OpenAI chat/completions 请求体转换为 OllamaChatRequest
// JSON。model/messages/stream 原样映射；max_tokens→options.num_predict；
// temperature→options.temperature；tools 直接映射（parameters 原样）。
func OpenAIChatToOllamaChat(openAIReq []byte) ([]byte, error) {
	var req openAIChatRequest
	if err := json.Unmarshal(openAIReq, &req); err != nil {
		return nil, fmt.Errorf("unmarshal openai chat request: %w", err)
	}

	out := &OllamaChatRequest{
		Model:  req.Model,
		Stream: req.Stream,
	}
	if len(req.Messages) > 0 {
		out.Messages = make([]OllamaChatMessage, 0, len(req.Messages))
		for _, m := range req.Messages {
			om, err := openAIChatMessageToOllama(m)
			if err != nil {
				return nil, err
			}
			out.Messages = append(out.Messages, om)
		}
	}
	if req.MaxTokens != nil || req.Temperature != nil {
		out.Options = make(map[string]any)
		if req.MaxTokens != nil {
			out.Options["num_predict"] = *req.MaxTokens
		}
		if req.Temperature != nil {
			out.Options["temperature"] = *req.Temperature
		}
	}
	if len(req.Tools) > 0 {
		out.Tools = make([]OllamaTool, 0, len(req.Tools))
		for _, t := range req.Tools {
			params := t.Function.Parameters
			if params == nil {
				params = map[string]any{}
			}
			out.Tools = append(out.Tools, OllamaTool{
				Type: t.Type,
				Function: OllamaToolFunction{
					Name:        t.Function.Name,
					Description: t.Function.Description,
					Parameters:  params,
				},
			})
		}
	}

	return json.Marshal(out)
}

// openAIChatMessageToOllama 把单条 OpenAI 消息转换为 OllamaChatMessage：内容块数组
// 拼成纯文本、image_url 抽到 images；tool_calls 的 arguments 由 JSON 字符串解析为对象。
func openAIChatMessageToOllama(m openAIChatMessage) (OllamaChatMessage, error) {
	content, images, err := openAIContentToOllama(m.Content)
	if err != nil {
		return OllamaChatMessage{}, err
	}
	om := OllamaChatMessage{Role: m.Role, Content: content, Images: images}
	for _, tc := range m.ToolCalls {
		args := map[string]any{}
		if s := strings.TrimSpace(tc.Function.Arguments); s != "" {
			if err := json.Unmarshal([]byte(s), &args); err != nil {
				args = map[string]any{} // 解析失败则用空对象，避免破坏对话
			}
		}
		om.ToolCalls = append(om.ToolCalls, OllamaToolCall{
			Function: OllamaToolCallFunction{Name: tc.Function.Name, Arguments: args},
		})
	}
	return om, nil
}

// openAIContentToOllama 把 OpenAI 消息内容（字符串或内容块数组）转为纯文本与图片列表。
func openAIContentToOllama(raw json.RawMessage) (string, []string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil, nil
	}
	var blocks []openAIContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", nil, fmt.Errorf("parse message content: %w", err)
	}
	var b strings.Builder
	var images []string
	for _, blk := range blocks {
		switch blk.Type {
		case "text":
			b.WriteString(blk.Text)
		case "image_url":
			if blk.ImageURL.URL != "" {
				images = append(images, blk.ImageURL.URL)
			}
		}
	}
	return b.String(), images, nil
}

// OllamaChatResponseToOpenAI 把 OllamaChatResponse 转换为 OpenAI chat.completion
// JSON。tool_calls 的 arguments 由对象序列化为 JSON 字符串；finish_reason 按
// stop/stop、length/max_tokens、tool_calls/tool_calls 映射；usage 对应 prompt 与 eval 计数。
func OllamaChatResponseToOpenAI(ollamaResp []byte, model string) ([]byte, error) {
	var resp OllamaChatResponse
	if err := json.Unmarshal(ollamaResp, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal ollama chat response: %w", err)
	}

	content := resp.Message.Content
	msg := openAIResponseMessage{Role: "assistant", Content: &content}
	for i, tc := range resp.Message.ToolCalls {
		args := "{}"
		if tc.Function.Arguments != nil {
			b, err := json.Marshal(tc.Function.Arguments)
			if err != nil {
				return nil, fmt.Errorf("marshal tool_call arguments: %w", err)
			}
			args = string(b)
		}
		msg.ToolCalls = append(msg.ToolCalls, openAIToolCall{
			ID:   fmt.Sprintf("call_%d", i),
			Type: "function",
			Function: openAIToolCallFunction{
				Name:      tc.Function.Name,
				Arguments: args,
			},
		})
	}

	total := resp.PromptEvalCount + resp.EvalCount
	out := openAICompletion{
		ID:      "chatcmpl-ollama",
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []openAIChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: ollamaFinishReason(resp.DoneReason, len(msg.ToolCalls) > 0),
		}},
		Usage: openAIUsage{
			PromptTokens:     resp.PromptEvalCount,
			CompletionTokens: resp.EvalCount,
			TotalTokens:      total,
		},
	}
	return json.Marshal(out)
}

// ollamaFinishReason 把 Ollama 的 done_reason 映射为 OpenAI 的 finish_reason。
func ollamaFinishReason(doneReason string, hasToolCalls bool) string {
	if hasToolCalls {
		return "tool_calls"
	}
	switch doneReason {
	case "length":
		return "max_tokens"
	default:
		return "stop"
	}
}

// OpenAIToOllamaEmbed 把 OpenAI /v1/embeddings 请求体转换为 OllamaEmbedRequest JSON。
func OpenAIToOllamaEmbed(openAIReq []byte) ([]byte, error) {
	var req struct {
		Model string `json:"model"`
		Input any    `json:"input"`
	}
	if err := json.Unmarshal(openAIReq, &req); err != nil {
		return nil, fmt.Errorf("unmarshal openai embed request: %w", err)
	}
	return json.Marshal(OllamaEmbedRequest{Model: req.Model, Input: req.Input})
}

// OllamaEmbedToOpenAI 把 OllamaEmbedResponse 转换为 OpenAI embeddings JSON。
// usage.prompt_tokens 来自 prompt_eval_count。
func OllamaEmbedToOpenAI(ollamaResp []byte) ([]byte, error) {
	var resp OllamaEmbedResponse
	if err := json.Unmarshal(ollamaResp, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal ollama embed response: %w", err)
	}
	data := make([]openAIEmbedding, 0, len(resp.Embeddings))
	for i, emb := range resp.Embeddings {
		data = append(data, openAIEmbedding{Object: "embedding", Embedding: emb, Index: i})
	}
	return json.Marshal(openAIEmbeddingResponse{
		Object: "list",
		Data:   data,
		Model:  resp.Model,
		Usage:  openAIEmbedUsage{PromptTokens: resp.PromptEvalCount, TotalTokens: resp.PromptEvalCount},
	})
}

// StreamOllamaChatToOpenAI 逐行读取 Ollama 的 NDJSON 流，转换为 OpenAI SSE
// （data: {...}\n\n）。message.content→delta.content，首行加 role=assistant，
// done=true 时输出 finish_reason 并追加 [DONE]。
func StreamOllamaChatToOpenAI(r io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	created := time.Now().Unix()
	model := ""
	roleSent := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var chunk OllamaChatResponse
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			return fmt.Errorf("unmarshal ollama stream chunk: %w", err)
		}
		if chunk.Model != "" && model == "" {
			model = chunk.Model
		}
		delta := openAIStreamDelta{Content: chunk.Message.Content}
		if !roleSent {
			delta.Role = "assistant"
			roleSent = true
		}
		var finish *string
		if chunk.Done {
			fr := ollamaFinishReason(chunk.DoneReason, len(chunk.Message.ToolCalls) > 0)
			finish = &fr
		}
		out := openAIStreamChunk{
			ID:      "chatcmpl-ollama",
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   model,
			Choices: []openAIStreamChoice{{Index: 0, Delta: delta, FinishReason: finish}},
		}
		if err := writeOpenAISSE(w, out); err != nil {
			return err
		}
		if chunk.Done {
			if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
				return err
			}
			flushWriter(w)
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

// StreamOpenAIToOllamaChat 逐行读取 OpenAI 的 SSE 流，转换为 Ollama NDJSON 流
// （反向）。delta.content→message.content；[DONE] 时输出 done=true 的收尾行。
func StreamOpenAIToOllamaChat(r io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	model := ""
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			out := OllamaChatResponse{
				Model:      model,
				Message:    OllamaChatMessage{Role: "assistant"},
				Done:       true,
				DoneReason: "stop",
			}
			if err := writeOllamaNDJSON(w, out); err != nil {
				return err
			}
			return nil
		}
		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return fmt.Errorf("unmarshal openai stream chunk: %w", err)
		}
		if chunk.Model != "" {
			model = chunk.Model
		}
		if len(chunk.Choices) == 0 {
			continue // 仅含 usage 的 chunk，跳过
		}
		delta := chunk.Choices[0].Delta
		role := delta.Role
		if role == "" {
			role = "assistant"
		}
		out := OllamaChatResponse{
			Model:   chunk.Model,
			Message: OllamaChatMessage{Role: role, Content: delta.Content},
			Done:    false,
		}
		if err := writeOllamaNDJSON(w, out); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

// writeOpenAISSE 以 "data: {...}\n\n" 格式写一个 OpenAI SSE 事件。
func writeOpenAISSE(w io.Writer, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
		return err
	}
	flushWriter(w)
	return nil
}

// writeOllamaNDJSON 以一行 JSON + 换行写一条 Ollama NDJSON 记录。
func writeOllamaNDJSON(w io.Writer, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if _, err := w.Write(b); err != nil {
		return err
	}
	flushWriter(w)
	return nil
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
