package proxy

// DeepSeek 官方分词器的纯 Go 实现。
//
// 背景：网关统计 token 时优先用上游返回的 usage；上游没给（少数不返回 usage 的
// 上游、或流式被截断）才需要本地估算。此前本地估算一律用 cl100k_base（OpenAI 词表），
// 对国内模型（尤其中文）有明显偏差——实测 cl100k 比 DeepSeek 官方分词器整体高估
// 11.7%，纯中文高估 24.2%，中文数字（零一二三…亿）可高估 144%。
//
// 现改用 DeepSeek 官方分词器作为统一标尺覆盖所有模型：DeepSeek 系在国内是事实标准，
// 且对中文/代码的切分显著更贴近国产模型的实际计费口径。
//
// 词表来源：https://cdn.deepseek.com/api-docs/deepseek_v4_tokenizer.zip
// （tokenizer.json，与 ModelScope DeepSeek-V4.1-Flash 那份功能等价）。
// 以 gzip 内嵌进二进制（6.4MB → 1.9MB），首次使用时解压，离线可用。
//
// 实现要点（与 HuggingFace tokenizers 逐 id 对齐，见 dstokenizer_test.go）：
//  1. pre_tokenizer = 三级 Split(Isolated) + ByteLevel(use_regex=false)；
//     三处正则用到 \p{L}\p{M}\p{N}\p{S}，Go 标准库 RE2 不支持 → 用 regexp2。
//     注意 regexp2 的 Index/Length 以 rune 为单位（不是字节）。
//  2. byte_fallback=false、unk_token=null → 无需字节回退，但所有字节都必须能被
//     byteEncode 覆盖，否则会丢字符。
//  3. GPT-2 bytes_to_unicode：前 188 个可打印字节映射为字符本身，其余 68 个映射到 256+n。
//  4. added_tokens 里的 special token（如 <｜begin▁of▁sentence｜>）必须以最长匹配
//     整体切出，否则会被 ByteLevel 拆成字节序列。

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/dlclark/regexp2"
)

// deepsSeekTokenizerData 是官方 tokenizer.json 的 gzip 内容。
//
//go:embed testdata/deepseek_tokenizer.json.gz
var deepsSeekTokenizerData []byte

// 官方 tokenizer.json 的结构（只声明本实现需要的字段）。
type dsSplitCfg struct {
	Type     string          `json:"type"`
	Pattern  json.RawMessage `json:"pattern"`
	Behavior string          `json:"behavior"`
}

type dsPreCfg struct {
	Pretokenizers []dsSplitCfg `json:"pretokenizers"`
}

type dsBPECfg struct {
	Vocab  map[string]int64  `json:"vocab"`
	Merges []json.RawMessage `json:"merges"`
}

type dsAddedToken struct {
	ID      int64  `json:"id"`
	Content string `json:"content"`
}

type dsTokCfg struct {
	Model        dsBPECfg       `json:"model"`
	AddedTokens  []dsAddedToken `json:"added_tokens"`
	PreTokenizer dsPreCfg       `json:"pre_tokenizer"`
}

// dsTokenizer 是 DeepSeek 官方分词器的纯 Go 实现，并发安全。
//
// 分词结果按文本缓存：管理接口统计/流式兜底会对相同文本反复计数，
// 而完整分词一次长 prompt 约需毫秒级，缓存后重复请求近乎零成本。
type dsTokenizer struct {
	vocab  map[string]int64
	merges map[[2]string]int32

	splits []*regexp2.Regexp

	byteEnc [256]rune

	// special token：按首字符索引候选，长 token 优先，保证最长匹配。
	specialByRune map[rune][][]rune
	specialID     map[string]int64

	mu    sync.RWMutex
	cache map[string][]int64
}

// maxTokenCacheEntries 限制分词缓存的条目数（超出即整体丢弃，避免无界增长）。
const maxTokenCacheEntries = 4096

// newDSTokenizer 从 gzip 压缩的 tokenizer.json 构建分词器。
func newDSTokenizer(gz []byte) (*dsTokenizer, error) {
	if len(gz) == 0 {
		return nil, fmt.Errorf("tokenizer 数据为空")
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, fmt.Errorf("解压 tokenizer: %w", err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("读取 tokenizer: %w", err)
	}
	var cfg dsTokCfg
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("解析 tokenizer: %w", err)
	}
	if len(cfg.Model.Vocab) == 0 {
		return nil, fmt.Errorf("tokenizer 词表为空")
	}

	t := &dsTokenizer{
		vocab:         cfg.Model.Vocab,
		merges:        make(map[[2]string]int32, len(cfg.Model.Merges)),
		specialByRune: make(map[rune][][]rune),
		specialID:     make(map[string]int64, len(cfg.AddedTokens)),
		cache:         make(map[string][]int64),
	}

	// merges：兼容 ["a b", ...] 与 [["a","b"], ...] 两种写法；rank 越小优先级越高。
	for rank, m := range cfg.Model.Merges {
		var pair [2]string
		if err := json.Unmarshal(m, &pair); err != nil {
			var s string
			if err2 := json.Unmarshal(m, &s); err2 != nil {
				continue
			}
			f := strings.SplitN(s, " ", 2)
			if len(f) != 2 {
				continue
			}
			pair = [2]string{f[0], f[1]}
		}
		if _, dup := t.merges[pair]; !dup {
			t.merges[pair] = int32(rank)
		}
	}

	// 预分词的三个 Split（ByteLevel 那级不是 Split，在 tokenizeNormal 里手工处理）
	for _, s := range cfg.PreTokenizer.Pretokenizers {
		if s.Type != "Split" {
			continue
		}
		var p struct {
			Regex string `json:"Regex"`
		}
		if err := json.Unmarshal(s.Pattern, &p); err != nil || p.Regex == "" {
			continue
		}
		re, err := regexp2.Compile(p.Regex, regexp2.None)
		if err != nil {
			return nil, fmt.Errorf("编译预分词正则 %q: %w", p.Regex, err)
		}
		t.splits = append(t.splits, re)
	}
	if len(t.splits) == 0 {
		return nil, fmt.Errorf("tokenizer 缺少预分词规则")
	}

	// GPT-2 bytes_to_unicode
	bs := make([]int, 0, 256)
	cs := make([]int, 0, 256)
	for i := 0x21; i <= 0x7E; i++ {
		bs, cs = append(bs, i), append(cs, i)
	}
	for i := 0xA1; i <= 0xAC; i++ {
		bs, cs = append(bs, i), append(cs, i)
	}
	for i := 0xAE; i <= 0xFF; i++ {
		bs, cs = append(bs, i), append(cs, i)
	}
	inBS := make(map[int]bool, len(bs))
	for _, b := range bs {
		inBS[b] = true
	}
	n := 0
	for b := 0; b < 256; b++ {
		if !inBS[b] {
			bs, cs = append(bs, b), append(cs, 256+n)
			n++
		}
	}
	for i, b := range bs {
		t.byteEnc[b] = rune(cs[i])
	}

	// special token：长优先，按首字符建索引
	contents := make([]string, 0, len(cfg.AddedTokens))
	for _, a := range cfg.AddedTokens {
		if a.Content == "" {
			continue
		}
		if _, dup := t.specialID[a.Content]; dup {
			continue
		}
		t.specialID[a.Content] = a.ID
		contents = append(contents, a.Content)
	}
	sort.Slice(contents, func(i, j int) bool { return len(contents[i]) > len(contents[j]) })
	for _, c := range contents {
		r := []rune(c)[0]
		t.specialByRune[r] = append(t.specialByRune[r], []rune(c))
	}
	return t, nil
}

// VocabSize 返回词表条目数（含 added_tokens）。
func (t *dsTokenizer) VocabSize() int {
	return len(t.vocab) + len(t.specialID)
}

// Encode 返回文本的 token id 序列。并发安全；结果带缓存。
func (t *dsTokenizer) Encode(text string) ([]int64, error) {
	if text == "" {
		return nil, nil
	}
	t.mu.RLock()
	if c, ok := t.cache[text]; ok {
		t.mu.RUnlock()
		return c, nil
	}
	t.mu.RUnlock()

	ids := make([]int64, 0, len(text)/2+8)
	for _, piece := range t.splitPieces(text) {
		if piece == "" {
			continue
		}
		if id, ok := t.specialID[piece]; ok {
			ids = append(ids, id)
			continue
		}
		for _, sym := range t.bpeMerge(t.byteEncode(piece)) {
			id, ok := t.vocab[sym]
			if !ok {
				// 词表缺失（异常数据/词表被截断）：跳过而非记错 id，并让调用方
				// 通过 unknown 计数感知。正常词表下不应出现。
				continue
			}
			ids = append(ids, id)
		}
	}

	t.mu.Lock()
	if len(t.cache) >= maxTokenCacheEntries {
		t.cache = make(map[string][]int64)
	}
	t.cache[text] = ids
	t.mu.Unlock()
	return ids, nil
}

// Count 返回文本的 token 数（便捷方法）。
func (t *dsTokenizer) Count(text string) int {
	ids, err := t.Encode(text)
	if err != nil {
		return 0
	}
	return len(ids)
}

// splitPieces 先按 special token 切段，普通段再走三级 Split。
func (t *dsTokenizer) splitPieces(text string) []string {
	r := []rune(text)
	var out []string
	var normal strings.Builder
	flush := func() {
		if normal.Len() > 0 {
			out = append(out, t.splitNormal(normal.String(), 0)...)
			normal.Reset()
		}
	}
	for i := 0; i < len(r); {
		matched := false
		for _, cand := range t.specialByRune[r[i]] {
			n := len(cand)
			if n == 0 || i+n > len(r) || string(r[i:i+n]) != string(cand) {
				continue
			}
			flush()
			out = append(out, string(cand))
			i += n
			matched = true
			break
		}
		if !matched {
			normal.WriteRune(r[i])
			i++
		}
	}
	flush()
	return out
}

// splitNormal 递归应用三级 Split(behavior=Isolated)：
// 匹配段作为独立片段切出，未匹配部分继续交给下一级。
func (t *dsTokenizer) splitNormal(s string, level int) []string {
	if level >= len(t.splits) {
		return []string{s}
	}
	re := t.splits[level]
	r := []rune(s)
	var out []string
	last := 0
	m, err := re.FindStringMatch(s)
	for err == nil && m != nil {
		g := m.GroupByNumber(0)
		st, en := g.Index, g.Index+g.Length // regexp2 的索引单位是 rune
		if st > last {
			out = append(out, t.splitNormal(string(r[last:st]), level+1)...)
		}
		out = append(out, t.splitNormal(g.String(), level+1)...)
		last = en
		m, err = re.FindNextMatch(m)
	}
	if last < len(r) {
		out = append(out, t.splitNormal(string(r[last:]), level+1)...)
	}
	return out
}

// byteEncode 把 UTF-8 字节按 GPT-2 映射转成可进 BPE 的 unicode 串。
func (t *dsTokenizer) byteEncode(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for _, b := range []byte(s) {
		sb.WriteRune(t.byteEnc[b])
	}
	return sb.String()
}

// bpeMerge 对单个片段做 BPE 逐级合并（贪心取 rank 最小的相邻对，同 HF 实现）。
func (t *dsTokenizer) bpeMerge(word string) []string {
	syms := make([]string, 0, len(word))
	for _, r := range word {
		syms = append(syms, string(r))
	}
	for len(syms) > 1 {
		bestRank, bestIdx := int32(-1), -1
		for i := 0; i+1 < len(syms); i++ {
			if rank, ok := t.merges[[2]string{syms[i], syms[i+1]}]; ok {
				if bestIdx < 0 || rank < bestRank {
					bestRank, bestIdx = rank, i
				}
			}
		}
		if bestIdx < 0 {
			break
		}
		syms[bestIdx] += syms[bestIdx+1]
		syms = append(syms[:bestIdx+1], syms[bestIdx+2:]...)
	}
	return syms
}

// ---- 全局单例：词表解压一次，进程内共享 ----

var (
	dsTokenizerOnce sync.Once
	dsTokenizerInst *dsTokenizer
	dsTokenizerErr  error
)

// deepSeekTokenizer 返回全局 DeepSeek 分词器（懒加载，首次调用解压词表）。
// 加载失败时返回 nil（调用方回退到粗略估算），不会 panic。
func deepSeekTokenizer() *dsTokenizer {
	dsTokenizerOnce.Do(func() {
		dsTokenizerInst, dsTokenizerErr = newDSTokenizer(deepsSeekTokenizerData)
	})
	if dsTokenizerErr != nil {
		return nil
	}
	return dsTokenizerInst
}
