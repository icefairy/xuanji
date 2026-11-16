// Package proxy 提供请求转发与上游选择。
package proxy

import (
	"crypto/sha1"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
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

// Tokenizer 提供 token 计数：优先 DeepSeek 官方分词器（统一标尺），
// 仅在分词器不可用时回退到 cl100k / 字符估算。
//
// 计量口径：上游返回的 usage 始终优先（见 proxy.go 调用点），本类型只用于
// 「上游没返回 usage」时的本地估算——例如不返回 usage 的上游、流式被截断等。
type Tokenizer struct {
	mu    sync.RWMutex
	cache map[string]*tiktoken.Tiktoken
}

// NewTokenizer 创建 Tokenizer。
func NewTokenizer() *Tokenizer {
	return &Tokenizer{cache: make(map[string]*tiktoken.Tiktoken)}
}

// Count 计算文本的 token 数。
//
// model 参数保留仅为兼容旧调用方与日志语义：DeepSeek 分词器对中文/代码的切分
// 贴近国产模型的实际计费口径，故统一用它计量所有模型（cl100k 对中文高估约 24%，
// 中文数字可高估 144%，见 dstokenizer.go 注释）。仅当分词器加载失败时，
// 才按模型名回退 tiktoken 编码器。
func (tz *Tokenizer) Count(model, text string) int {
	if ds := deepSeekTokenizer(); ds != nil {
		return ds.Count(text)
	}
	// 兜底：DeepSeek 分词器不可用（词表损坏/内存不足）时才走 tiktoken。
	tke, err := tz.getEncoding(model)
	if err != nil {
		// 回退估算
		return estimateTokens(text)
	}
	return len(tke.Encode(text, nil, nil))
}

// CountMessages 计算消息列表的 token 数。
//
// 说明：OpenAI 的「每条消息 +4、结尾 +2」是 cl100k 的对话格式开销；DeepSeek 用的是
// 自己的模板（<｜begin▁of▁sentence｜>{system}<｜User｜>{content}<｜Assistant｜>）。
// 这里按 DeepSeek 模板的边界 token 计，比把消息当裸文本相加更贴近上游口径。
// 该路径只在上游未返回 usage 时生效，属兜底估算而非精确计费。
func (tz *Tokenizer) CountMessages(model string, messages []map[string]string) int {
	total := 0
	for _, msg := range messages {
		// 角色标记（<｜User｜> / <｜Assistant｜> 等）各算 1 个 token
		total += 1
		if role := msg["role"]; role != "system" && role != "user" && role != "assistant" && role != "tool" {
			total += tz.Count(model, role)
		}
		total += tz.Count(model, msg["content"])
	}
	// 起始 <｜begin▁of▁sentence｜> 与结尾 <｜Assistant｜>
	total += 2
	return total
}

// getEncoding 获取模型对应的 tiktoken 编码器（仅作 DeepSeek 分词器不可用时的兜底）。
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
// 兜底路径已统一用 cl100k_base：DeepSeek 分词器覆盖正常情况，
// 这里只在词表加载失败时临时顶替，按模型细分编码器已无收益。
func modelToEncoding(model string) string {
	return "cl100k_base"
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
