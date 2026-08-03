package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/router"
)

// TestFastFail_OneToManyMappingIsolation 验证一对多映射下 fastfail 按真实模型名隔离：
// 商汤 deepseek-v4-flash -> "sensenova-6.7-flash-lite|deepseek-v4-flash"
// Lite 真实模型 429 被拉黑后，deepseek-v4-flash 真实模型仍可用 → 请求应成功（映射到可用的那个）。
func TestFastFail_OneToManyMappingIsolation(t *testing.T) {
	var called atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Add(1)
		// 返回带真实模型名的响应，便于断言走了哪个映射
		model := ""
		b, err := io.ReadAll(r.Body)
		if err == nil {
			var req struct {
				Model string `json:"model"`
			}
			if json.Unmarshal(b, &req) == nil {
				model = req.Model
			}
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"id":"ok","object":"chat.completion","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`, model)
	}))
	defer up.Close()

	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{
				Name: "sensetime", BaseURL: up.URL, APIKey: "sk-test",
				Priority: 1,
				Models:   []string{"deepseek-v4-flash"},
				ModelMapping: map[string]string{
					"deepseek-v4-flash": "sensenova-6.7-flash-lite|deepseek-v4-flash",
				},
			},
		},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules: []config.Rule{
				{Model: "deepseek-v4-flash", Upstreams: []string{"sensetime"}, Strategy: "primary_backup"},
			},
		},
		Retry: config.Retry{MaxRetries: 3, RetryStatuses: []int{429, 500}},
	}
	h := New(cfg, router.New(cfg), nil)
	ff := NewFastFailCache(time.Minute)
	h.SetFastFail(ff)

	// 1. 先拉黑 Lite 真实模型 → 仍应能请求成功（映射到 deepseek-v4-flash）
	ff.MarkFailed("sensetime", "sensenova-6.7-flash-lite")
	rec := doChat(t, h, `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("Lite blacklisted but request failed: %d %s", rec.Code, rec.Body.String())
	}
	if called.Load() == 0 {
		t.Fatal("upstream should be called when one mapped model is still available")
	}
	// 断言响应里 model 是 deepseek-v4-flash（被拉黑的 Lite 不应被选中）
	if !strings.Contains(rec.Body.String(), `"model":"deepseek-v4-flash"`) {
		t.Fatalf("expected request to map to deepseek-v4-flash (Lite blacklisted), got body: %s", rec.Body.String())
	}

	// 2. 两个真实模型都拉黑 → 全黑时 selectCandidates 不缩减 → fallback 原列表 → forwardOnce
	//    退化为 MapModel 继续尝试（黑名单是软跳过，真实请求成功可纠正黑名单）。
	called.Store(0)
	ff.MarkFailed("sensetime", "deepseek-v4-flash")
	rec2 := doChat(t, h, `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`)
	if called.Load() == 0 {
		t.Fatal("fallback should still attempt upstream when all mapped models blacklisted")
	}
	// 3. 请求成功后被选中的真实模型应被清除（MarkSuccess）；另一个保持黑名单（隔离性）
	if ff.IsBlacklisted("sensetime", "deepseek-v4-flash") && ff.IsBlacklisted("sensetime", "sensenova-6.7-flash-lite") {
		t.Fatal("at least one mapped model should be cleared after successful fallback request")
	}
	_ = rec2
}
