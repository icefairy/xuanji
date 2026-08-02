// Package proxy 提供请求转发与上游选择。
package proxy

import (
	_ "embed"
	"crypto/sha1"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	tiktoken "github.com/pkoukk/tiktoken-go"
)

//go:embed cl100k_base.tiktoken
var cl100kBaseData []byte

// cl100kBaseBlobpath 是 tiktoken-go 用于缓存 key 的 URL。
const cl100kBaseBlobpath = "https://openaipublic.blob.core.windows.net/encodings/cl100k_base.tiktoken"

// init 将内置的 cl100k_base.tiktoken 释放到缓存目录，确保离线可用。
func init() {
	cacheDir := os.Getenv("TIKTOKEN_CACHE_DIR")
	if cacheDir == "" {
		cacheDir = os.Getenv("DATA_GYM_CACHE_DIR")
	}
	if cacheDir == "" {
		cacheDir = filepath.Join(os.TempDir(), "data-gym-cache")
	}
	cacheKey := fmt.Sprintf("%x", sha1.Sum([]byte(cl100kBaseBlobpath)))
	cachePath := filepath.Join(cacheDir, cacheKey)
	if _, err := os.Stat(cachePath); os.IsNotExist(err) {
		os.MkdirAll(cacheDir, 0755)
		os.WriteFile(cachePath, cl100kBaseData, 0644)
	}
}

// Tokenizer 封装 tiktoken，提供模型 token 计数。
type Tokenizer struct {
	mu    sync.RWMutex
	cache map[string]*tiktoken.Tiktoken
}

// NewTokenizer 创建 Tokenizer。
func NewTokenizer() *Tokenizer {
	return &Tokenizer{cache: make(map[string]*tiktoken.Tiktoken)}
}

// Count 计算文本的 token 数。model 为模型名（如 deepseek-v4-flash）。
// 使用模型对应的编码器；未知模型回退到 cl100k_base。
func (tz *Tokenizer) Count(model, text string) int {
	tke, err := tz.getEncoding(model)
	if err != nil {
		// 回退估算
		return estimateTokens(text)
	}
	return len(tke.Encode(text, nil, nil))
}

// CountMessages 计算消息列表的 token 数（近似 OpenAI 的 per-message 格式）。
func (tz *Tokenizer) CountMessages(model string, messages []map[string]string) int {
	total := 0
	for _, msg := range messages {
		total += 4 // 每条消息的格式开销
		total += tz.Count(model, msg["role"])
		total += tz.Count(model, msg["content"])
	}
	total += 2 // 回复开销
	return total
}

// getEncoding 获取模型对应的 tiktoken 编码器。
func (tz *Tokenizer) getEncoding(model string) (*tiktoken.Tiktoken, error) {
	name := modelToEncoding(model)
	tz.mu.RLock()
	tke, ok := tz.cache[name]
	tz.mu.RUnlock()
	if ok {
		return tke, nil
	}
	tke, err := tiktoken.GetEncoding(name)
	if err != nil {
		return nil, err
	}
	tz.mu.Lock()
	tz.cache[name] = tke
	tz.mu.Unlock()
	return tke, nil
}

// modelToEncoding 将模型名映射到 tiktoken 编码器名。
func modelToEncoding(model string) string {
	lower := strings.ToLower(model)
	switch {
	case strings.Contains(lower, "gpt-4"):
		return "cl100k_base"
	case strings.Contains(lower, "gpt-3.5"):
		return "cl100k_base"
	case strings.Contains(lower, "text-embedding-ada"):
		return "cl100k_base"
	case strings.Contains(lower, "deepseek"):
		return "cl100k_base" // deepseek 兼容 OpenAI
	case strings.Contains(lower, "qwen"):
		return "cl100k_base" // qwen 兼容
	case strings.Contains(lower, "glm"):
		return "cl100k_base"
	case strings.Contains(lower, "agnes"):
		return "cl100k_base"
	case strings.Contains(lower, "mimo"):
		return "cl100k_base"
	case strings.Contains(lower, "sensenova"):
		return "cl100k_base"
	default:
		return "cl100k_base"
	}
}

// estimateTokens 粗略估算（兜底 fallback）。
func estimateTokens(text string) int {
	chinese := 0
	other := 0
	for _, r := range text {
		if r > 0x4E00 && r < 0x9FFF {
			chinese++
		} else {
			other++
		}
	}
	return int(float64(chinese)/1.5 + float64(other)/4 + 0.5)
}
