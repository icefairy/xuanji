package proxy

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// tokenizerCase 是一条基准样本（文本 → 官方 token id 序列）。
type tokenizerCase struct {
	Text string  `json:"text"`
	IDs  []int64 `json:"ids"`
}

// loadTokenizerCases 读取 testdata 里的官方基准（由 scripts/gen_tokenizer_cases.py 生成）。
func loadTokenizerCases(t *testing.T) []tokenizerCase {
	t.Helper()
	f, err := os.Open("testdata/tokenizer_cases.json.gz")
	if err != nil {
		t.Fatalf("open cases: %v", err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var cases []tokenizerCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("no cases")
	}
	return cases
}

// TestDSEncode_MatchesOfficial 逐 id 比对官方分词器输出。
// 这是本实现的核心保证：任何偏差都必须在这里暴露。
func TestDSEncode_MatchesOfficial(t *testing.T) {
	ds, err := newDSTokenizer(deepsSeekTokenizerData)
	if err != nil {
		t.Fatalf("load tokenizer: %v", err)
	}
	cases := loadTokenizerCases(t)

	totalTokens, mismatches, unknown := 0, 0, 0
	for _, c := range cases {
		got, err := ds.Encode(c.Text)
		if err != nil {
			t.Fatalf("encode %q: %v", truncateForTest(c.Text), err)
		}
		totalTokens += len(c.IDs)
		if len(got) != len(c.IDs) {
			mismatches++
			if mismatches <= 5 {
				t.Errorf("长度不一致 %q: got %d want %d\n  got =%v\n  want=%v",
					truncateForTest(c.Text), len(got), len(c.IDs), got, c.IDs)
			}
			continue
		}
		for i := range got {
			if got[i] < 0 {
				unknown++
			}
			if got[i] != c.IDs[i] {
				mismatches++
				if mismatches <= 5 {
					t.Errorf("token 不一致 %q 位置 %d: got %d want %d",
						truncateForTest(c.Text), i, got[i], c.IDs[i])
				}
				break
			}
		}
	}
	if mismatches > 0 {
		t.Fatalf("%d/%d 样本不一致（共 %d token）", mismatches, len(cases), totalTokens)
	}
	if unknown > 0 {
		t.Fatalf("有 %d 个 token 无法映射到 id（词表缺失）", unknown)
	}
	t.Logf("全部一致：%d 样本 / %d token", len(cases), totalTokens)
}

// TestDSTokenizerLoadFailure 词表损坏时必须返回错误（而非静默给错数字）。
func TestDSTokenizerLoadFailure(t *testing.T) {
	if _, err := newDSTokenizer([]byte("not a gzip stream")); err == nil {
		t.Fatal("损坏的 tokenizer 数据应当报错")
	}
	if _, err := newDSTokenizer([]byte{}); err == nil {
		t.Fatal("空数据应当报错")
	}
}

// TestDSTokenizerDataValid 内置词表能被正常解压解析（防打包时漏文件/截断）。
func TestDSTokenizerDataValid(t *testing.T) {
	if len(deepsSeekTokenizerData) == 0 {
		t.Fatal("内置 tokenizer 数据为空")
	}
	ds, err := newDSTokenizer(deepsSeekTokenizerData)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if n := ds.VocabSize(); n < 120000 {
		t.Fatalf("词表过小: %d（疑似被截断）", n)
	}
}

// TestDSTokenizerSpecialTokens 官方 special token 必须整体匹配（不被拆成字节）。
func TestDSTokenizerSpecialTokens(t *testing.T) {
	ds, err := newDSTokenizer(deepsSeekTokenizerData)
	if err != nil {
		t.Fatal(err)
	}
	// 官方定义：<｜begin▁of▁sentence｜>=0，<｜end▁of▁sentence｜>=1
	got, err := ds.Encode("<｜begin▁of▁sentence｜>")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != 0 {
		t.Fatalf("<｜begin▁of▁sentence｜> 应编码为单个 id 0，得到 %v", got)
	}
	got, err = ds.Encode("<｜end▁of▁sentence｜>")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("<｜end▁of▁sentence｜> 应编码为单个 id 1，得到 %v", got)
	}
	// 混合文本：special + 普通文本
	got, err = ds.Encode("<｜User｜>hi")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("<｜User｜>hi 应编码为 2 个 token，得到 %v", got)
	}
}

func truncateForTest(s string) string {
	r := []rune(s)
	if len(r) > 40 {
		return string(r[:40]) + "..."
	}
	return s
}

// TestTokenizer_DeepSeekUnified 验证所有模型名都走 DeepSeek 分词器（统一标尺），
// 且分词结果与模型名无关（不再按模型选不同编码器）。
func TestTokenizer_DeepSeekUnified(t *testing.T) {
	tz := NewTokenizer()
	texts := []string{
		"请帮我统计今天用了多少 token",
		"func main() { fmt.Println(\"hello\") }",
		"零一二三四五六七八九十百千万亿",
	}
	models := []string{"deepseek-v4.1-flash", "glm-5.3-flash", "qwen3-max", "kimi-k2", "agnes-3.0-flash", ""}
	for _, text := range texts {
		base := tz.Count(models[0], text)
		if base == 0 {
			t.Fatalf("Count(%q) = 0，分词器未生效", text)
		}
		for _, m := range models[1:] {
			if got := tz.Count(m, text); got != base {
				t.Errorf("模型 %q 的计数 %d 与 %q 的 %d 不一致（应统一用同一分词器）", m, got, models[0], base)
			}
		}
	}
}

// TestTokenizer_ChineseNotOverestimated 验证中文不再被 cl100k 高估。
// 数值由官方分词器（Python tokenizers）实测得出，一旦回退到 cl100k 就会失败。
func TestTokenizer_ChineseNotOverestimated(t *testing.T) {
	tz := NewTokenizer()
	cases := []struct {
		text string
		want int // 官方 DeepSeek 分词器结果（cl100k 分别高估 33% / 86% / 144%）
	}{
		{strings.Repeat("中文测试。", 40), 120},
		{"请帮我统计今天用了多少 token", 7},
		{"零一二三四五六七八九十百千万亿", 9},
	}
	for _, c := range cases {
		if got := tz.Count("m", c.text); got != c.want {
			t.Errorf("Count(%q) = %d, want %d（若明显偏大说明回退到了 cl100k）",
				truncateForTest(c.text), got, c.want)
		}
	}
}

// TestStreamCopy_ModelArbitraryUsesDeepSeekTokenizer 验证非 DeepSeek 模型名
// 也走 DeepSeek 分词器（生产上 arti/agnes/glm 等混合流量都靠这条兜底）。
func TestStreamCopy_ModelArbitraryUsesDeepSeekTokenizer(t *testing.T) {
	h := &Handler{log: slog.New(slog.NewTextHandler(io.Discard, nil)), tokenizer: NewTokenizer()}
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"中文回复内容\"}}]}\n\n" +
		"data: [DONE]\n\n"

	// 用非 deepseek 的模型名（glm / agnes），本地估算照样生效
	for _, model := range []string{"glm-5.3-flash", "agnes-3.0-flash", "qwen3-max"} {
		// 每次新建 resp：Body 是读一次就耗尽的 reader，复用会导致后续轮次拿不到内容
		resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(sse))}
		rec := httptest.NewRecorder()
		body := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"你好"}]}`)
		var pt, ct int64
		_, _, _ = h.streamCopy(rec, resp, body, model, &pt, &ct, nil, nil, time.Now(), nil, nil)
		if pt <= 0 || ct <= 0 {
			t.Errorf("模型 %s 应有本地估算兜底，得到 (%d,%d)", model, pt, ct)
		}
	}
}
