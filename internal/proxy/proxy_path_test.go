package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/health"
	"github.com/icefairy/xuanji/internal/router"
)

// TestE2E_NonStandardBaseURL 验证非标准 BaseURL 前缀拼接（火山豆包 /api/v3、智谱 /api/paas/v4、百度千帆 /v2）
// 新代码不再补 /v1，BaseURL 末尾已是完整版本前缀 → 直接拼 suffix。
func TestE2E_NonStandardBaseURL(t *testing.T) {
	cases := []struct {
		name     string
		prefix   string // 拼在 srv.URL 后面的路径
		wantPath string
	}{
		{
			name:     "火山豆包 /api/v3",
			prefix:   "/api/v3",
			wantPath: "/api/v3/chat/completions",
		},
		{
			name:     "智谱 /api/paas/v4",
			prefix:   "/api/paas/v4",
			wantPath: "/api/paas/v4/chat/completions",
		},
		{
			name:     "百度千帆 /v2",
			prefix:   "/v2",
			wantPath: "/v2/chat/completions",
		},
		{
			name:     "标准 OpenAI /v1",
			prefix:   "/v1",
			wantPath: "/v1/chat/completions",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			var gotModel string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				b, _ := io.ReadAll(r.Body)
				var reqBody struct{ Model string `json:"model"` }
				if json.Unmarshal(b, &reqBody) == nil {
					gotModel = reqBody.Model
				}
				w.WriteHeader(http.StatusOK)
				fmt.Fprintf(w, `{"id":"ok","object":"chat.completion","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`, gotModel)
			}))
			defer srv.Close()

			realBaseURL := srv.URL + tc.prefix
			cfg := &config.Config{
				Upstreams: []config.Upstream{
					{
						Name: "test-up", BaseURL: realBaseURL, APIKey: "sk-test",
						Priority: 1, Weight: 100,
						Models: []string{"deepseek-v4-flash"},
					},
				},
				Routing: config.Routing{
					DefaultStrategy: "primary_backup",
					Rules: []config.Rule{
						{Model: "deepseek-v4-flash", Upstreams: []string{"test-up"}, Strategy: "primary_backup"},
					},
				},
				Retry: config.Retry{MaxRetries: 3, RetryStatuses: []int{429, 500, 502, 503, 504}},
			}
			h := New(cfg, router.New(cfg), health.New(cfg))

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer sk-test")
			rec := httptest.NewRecorder()
			h.ChatCompletions(rec, req)

			if rec.Code != 200 {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if gotPath != tc.wantPath {
				t.Fatalf("received path=%q, want %q", gotPath, tc.wantPath)
			}
		})
	}
}
