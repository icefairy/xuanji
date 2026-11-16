// Package gemini 实现 Google Gemini 协议入口：请求转 OpenAI 转发，响应转回 Gemini。
package gemini

// Gemini 消息角色。
const (
	RoleUser  = "user"
	RoleModel = "model"
)

// Gemini 完成原因（finishReason）。
const (
	FinishReasonStop        = "STOP"
	FinishReasonMaxTokens   = "MAX_TOKENS"
	FinishReasonSafety      = "SAFETY"
	FinishReasonRecitation  = "RECITATION"
	FinishReasonMalformedFn = "MALFORMED_FUNCTION_CALL"
	FinishReasonOther       = "OTHER"
)

// GeminiRequest 对应 generateContent / streamGenerateContent 请求体。
type GeminiRequest struct {
	Contents          []GeminiContent         `json:"contents,omitempty"`
	SystemInstruction *GeminiContent          `json:"systemInstruction,omitempty"`
	Tools             []GeminiTool            `json:"tools,omitempty"`
	ToolConfig        *GeminiToolConfig       `json:"toolConfig,omitempty"`
	GenerationConfig  *GeminiGenerationConfig `json:"generationConfig,omitempty"`
	Model             string                  `json:"model,omitempty"` // 部分 SDK 会在 body 里带 model
}

// GeminiContent 是一段对话内容（按 role 归属 user 或 model）。
type GeminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []GeminiPart `json:"parts,omitempty"`
}

// GeminiPart 是内容块，按字段区分文本 / 内联数据 / 文件引用 / 函数调用 / 函数结果。
type GeminiPart struct {
	Text             string                  `json:"text,omitempty"`
	InlineData       *GeminiBlob             `json:"inlineData,omitempty"`
	FileData         *GeminiFileData         `json:"fileData,omitempty"`
	FunctionCall     *GeminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *GeminiFunctionResponse `json:"functionResponse,omitempty"`
}

// GeminiBlob 是内联二进制数据（base64）。
type GeminiBlob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"` // base64 编码
}

// GeminiFileData 是文件引用（Google 托管文件）。
type GeminiFileData struct {
	MimeType string `json:"mimeType,omitempty"`
	FileURI  string `json:"fileUri,omitempty"`
}

// GeminiFunctionCall 是模型发起的函数调用。
type GeminiFunctionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

// GeminiFunctionResponse 是工具执行结果返回。
type GeminiFunctionResponse struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

// GeminiTool 定义模型可用的工具集合。
type GeminiTool struct {
	FunctionDeclarations []GeminiFunctionDecl `json:"functionDeclarations,omitempty"`
}

// GeminiFunctionDecl 是单个函数声明。
type GeminiFunctionDecl struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// GeminiToolConfig 控制函数调用行为。
type GeminiToolConfig struct {
	FunctionCallingConfig *GeminiFunctionCallingConfig `json:"functionCallingConfig,omitempty"`
}

// GeminiFunctionCallingConfig 是函数调用模式配置。
type GeminiFunctionCallingConfig struct {
	Mode                 string   `json:"mode,omitempty"` // AUTO / ANY / NONE
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

// GeminiGenerationConfig 是生成参数。
type GeminiGenerationConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	TopK            *int     `json:"topK,omitempty"`
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
	ResponseMimeType string  `json:"responseMimeType,omitempty"`
	CandidateCount  *int     `json:"candidateCount,omitempty"`
}

// GeminiResponse 对应非流式 generateContent 响应体。
type GeminiResponse struct {
	Candidates     []GeminiCandidate   `json:"candidates,omitempty"`
	UsageMetadata  *GeminiUsageMetadata `json:"usageMetadata,omitempty"`
	ModelVersion   string              `json:"modelVersion,omitempty"`
}

// GeminiCandidate 是单个生成候选。
type GeminiCandidate struct {
	Content      *GeminiContent `json:"content,omitempty"`
	FinishReason string         `json:"finishReason,omitempty"`
	Index        int            `json:"index,omitempty"`
}

// GeminiUsageMetadata 记录 token 用量。
type GeminiUsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount,omitempty"`
	CandidatesTokenCount int `json:"candidatesTokenCount,omitempty"`
	TotalTokenCount      int `json:"totalTokenCount,omitempty"`
}

// GeminiError 对应 Gemini API 错误响应结构。
type GeminiError struct {
	Error GeminiErrorDetail `json:"error"`
}

// GeminiErrorDetail 是错误详情。
type GeminiErrorDetail struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}