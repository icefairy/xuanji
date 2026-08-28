package proxy

import (
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/store"
)

// TestExtractMaxFromError 验证从上游 400 错误信息提取"at most N"的能力。
func TestExtractMaxFromError(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		want  int
		field string
	}{
		{
			name: "max_completion_tokens too large",
			body: `{"object":"error","message":"max_completion_tokens is too large: 963669. This model supports at most 262144 completion tokens.","type":"BadRequestError","code":400}`,
			want: 262144, field: "max_completion_tokens",
		},
		{
			name: "max_tokens too large",
			body: `{"error":{"message":"max_tokens is too large: 999999. The maximum allowed is 128000.","type":"invalid_request_error"}}`,
			want: 128000, field: "max_tokens",
		},
		{
			name: "not a limit error",
			body: `{"error":{"message":"Unknown model","type":"invalid_request_error"}}`,
			want: 0, field: "",
		},
		{
			name: "context length error (should NOT learn)",
			body: `{"error":{"message":"This model's maximum context length is 262144 tokens. However, your messages resulted in 300000 tokens.","type":"invalid_request_error"}}`,
			want: 0, field: "",
		},
		{
			name: "with are instead of is (grammar variation)",
			body: `{"error":{"message":"max_tokens are too large: 999999. The maximum allowed is 128000."}}`,
			want: 128000, field: "max_tokens",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, field := extractMaxFromError([]byte(c.body))
			if got != c.want || field != c.field {
				t.Fatalf("extractMaxFromError = (%d, %q), want (%d, %q)", got, field, c.want, c.field)
			}
		})
	}
}

// TestTokenLimitLearnAndApply 端到端验证：400 错误学习上限 → 后续请求自动 clamp。
func TestTokenLimitLearnAndApply(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	h := &Handler{tokenLimits: s, log: slog.Default()}
	up := &config.Upstream{Name: "testUp", BaseURL: "http://example.com/v1"}

	// 学习：从 400 错误提取 262144
	badResp := []byte(`{"object":"error","message":"max_completion_tokens is too large: 963669. This model supports at most 262144 completion tokens.","type":"BadRequestError","code":400}`)
	h.learnTokenLimit(up, "deepseek-v4-flash", "deepseek-v4-flash", badResp)

	// 验证已存储
	row, err := s.GetTokenLimit("testUp", "deepseek-v4-flash")
	if err != nil || row == nil {
		t.Fatalf("GetTokenLimit = %v, err=%v; want learned row", row, err)
	}
	if row.MaxCompletionTokens != 262144 {
		t.Fatalf("MaxCompletionTokens = %d, want 262144", row.MaxCompletionTokens)
	}

	// clamp：超过上限的 max_completion_tokens 被修改
	body := []byte(`{"model":"deepseek-v4-flash","max_completion_tokens":500000,"messages":[{"role":"user","content":"hi"}]}`)
	nb, changed := h.applyTokenLimit(body, up, "deepseek-v4-flash")
	if !changed {
		t.Fatal("applyTokenLimit should change body")
	}
	if got := string(nb); got != `{"model":"deepseek-v4-flash","max_completion_tokens":262144,"messages":[{"role":"user","content":"hi"}]}` {
		t.Fatalf("clamped body = %s", got)
	}

	// 未超限：不动
	body2 := []byte(`{"model":"deepseek-v4-flash","max_completion_tokens":100000,"messages":[{"role":"user","content":"hi"}]}`)
	if nb2, ch := h.applyTokenLimit(body2, up, "deepseek-v4-flash"); ch || string(nb2) != string(body2) {
		t.Fatalf("under-limit body must pass through unchanged, changed=%v", ch)
	}

	// 删除后：不再 clamp
	if err := s.DeleteTokenLimit("testUp", "deepseek-v4-flash"); err != nil {
		t.Fatal(err)
	}
	if nb3, ch := h.applyTokenLimit(body, up, "deepseek-v4-flash"); ch || string(nb3) != string(body) {
		t.Fatalf("after delete, body must pass through unchanged, changed=%v", ch)
	}
}

// TestTokenLimitSkipNonLimitError 验证：非"is too large"类 400 错误不学习。
func TestTokenLimitSkipNonLimitError(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	h := &Handler{tokenLimits: s, log: slog.Default()}
	up := &config.Upstream{Name: "testUp", BaseURL: "http://example.com/v1"}

	// 上下文超长错误 → 不学习（不应记录上限）
	ctxErr := []byte(`{"error":{"message":"This model's maximum context length is 262144 tokens. However, your messages resulted in 300000 tokens.","type":"invalid_request_error"}}`)
	h.learnTokenLimit(up, "deepseek-v4-flash", "deepseek-v4-flash", ctxErr)
	row, _ := s.GetTokenLimit("testUp", "deepseek-v4-flash")
	if row != nil {
		t.Fatalf("context length error should NOT be learned, got row with max=%d", row.MaxCompletionTokens)
	}

	// 未知模型错误 → 不学习
	unknownErr := []byte(`{"error":{"message":"Model 'xyz' not found.","type":"invalid_request_error"}}`)
	h.learnTokenLimit(up, "xyz", "xyz", unknownErr)
	row, _ = s.GetTokenLimit("testUp", "xyz")
	if row != nil {
		t.Fatalf("unknown model error should NOT be learned")
	}
}