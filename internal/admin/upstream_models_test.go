package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/icefairy/xuanji/internal/config"
)

// TestUpstreamModelsGemini 验证 gemini 上游"获取模型列表"：
// 请求带 x-goog-api-key 头，解析 Gemini 格式 models[].name 并剥掉 models/ 前缀。
func TestUpstreamModelsGemini(t *testing.T) {
	var gotKey, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-goog-api-key")
		gotAuth = r.Header.Get("Authorization")
		if !strings.HasSuffix(r.URL.Path, "/models") {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{
				{"name": "models/gemini-2.0-flash", "displayName": "Gemini 2.0 Flash"},
				{"name": "models/gemini-pro", "displayName": "Gemini Pro"},
			},
		})
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.Upstreams = append(cfg.Upstreams, config.Upstream{
		Name: "google-gemini", Type: "gemini", BaseURL: srv.URL + "/v1beta", APIKey: "gk-123",
	})
	h, _ := newTestHandler(t, cfg)

	r := httptest.NewRequest(http.MethodGet, "/admin/upstreams/google-gemini/models", nil)
	r.SetPathValue("name", "google-gemini")
	rr := httptest.NewRecorder()
	h.UpstreamModels(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if gotKey != "gk-123" {
		t.Errorf("x-goog-api-key = %q, want gk-123", gotKey)
	}
	if gotAuth != "" {
		t.Errorf("Authorization 不应携带（gemini 用 x-goog-api-key），got %q", gotAuth)
	}
	var resp struct {
		Status string   `json:"status"`
		Models []string `json:"models"`
		Count  int      `json:"count"`
	}
	decodeBody(t, rr, &resp)
	want := []string{"gemini-2.0-flash", "gemini-pro"}
	if len(resp.Models) != 2 || resp.Models[0] != want[0] || resp.Models[1] != want[1] {
		t.Errorf("models = %v, want %v", resp.Models, want)
	}
	if resp.Count != 2 {
		t.Errorf("count = %d, want 2", resp.Count)
	}
}

// TestUpstreamModelsOpenAI 回归：openai 上游仍走 Bearer 鉴权 + data[].id 解析。
func TestUpstreamModelsOpenAI(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "deepseek-v4-flash"},
				{"id": "bge-m3"},
			},
		})
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.Upstreams = append(cfg.Upstreams, config.Upstream{
		Name: "mock-openai", Type: "openai", BaseURL: srv.URL + "/v1", APIKey: "sk-abc",
	})
	h, _ := newTestHandler(t, cfg)

	r := httptest.NewRequest(http.MethodGet, "/admin/upstreams/mock-openai/models", nil)
	r.SetPathValue("name", "mock-openai")
	rr := httptest.NewRecorder()
	h.UpstreamModels(rr, r)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if gotAuth != "Bearer sk-abc" {
		t.Errorf("Authorization = %q, want Bearer sk-abc", gotAuth)
	}
	var resp struct {
		Models []string `json:"models"`
	}
	decodeBody(t, rr, &resp)
	if len(resp.Models) != 2 || resp.Models[0] != "deepseek-v4-flash" {
		t.Errorf("models = %v", resp.Models)
	}
}