package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/icefairy/xuanji/internal/config"
)

// TestTestUpstream_PipeModelMapping 验证 bug 修复：
// 测试上游 chat 分支在应用 model_mapping 后，对含竖线 "|" 的聚合 key
// （如 "mimo-v2.5-free|hy3-free"）只取第一个真实模型名发给上游，
// 而不是把整串带 "|" 的串发出去（否则上游报 ModelError: model xxx|yyy is not supported）。
func TestTestUpstream_PipeModelMapping(t *testing.T) {
	const wantRealModel = "mimo-v2.5-free" // 竖线后的 hy3-free 不应出现

	var gotModel string
	var gotAuth string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		gotModel = req.Model
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer mock.Close()

	cfg := &config.Config{
		Server: config.Server{Port: 8787},
		Upstreams: []config.Upstream{
			{
				Name:         "pipe-up",
				Type:         "openai",
				BaseURL:      mock.URL,
				APIKey:       "sk-test",
				Models:       []string{"free-agg"},
				ModelMapping: map[string]string{"free-agg": "mimo-v2.5-free|hy3-free"},
			},
		},
	}
	h, hc := newTestHandler(t, cfg)
	defer hc.Close()

	rr := httptest.NewRecorder()
	payload, _ := json.Marshal(map[string]string{"model": "free-agg"})
	req := httptest.NewRequest(http.MethodPost, "/admin/upstreams/pipe-up/test", strings.NewReader(string(payload)))
	req.SetPathValue("name", "pipe-up")

	h.TestUpstream(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if gotModel != wantRealModel {
		t.Errorf("upstream received model %q, want %q (竖线多模型映射应只取第一个真实模型)", gotModel, wantRealModel)
	}
	if gotModel != "" && strings.Contains(gotModel, "|") {
		t.Errorf("upstream received model %q 仍含竖线，应已拆分", gotModel)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer sk-test")
	}
}

// TestTestUpstream_PlainModelMapping 回归：无竖线映射仍原样透传真实模型名。
func TestTestUpstream_PlainModelMapping(t *testing.T) {
	const wantRealModel = "deepseek-v4-flash"

	var gotModel string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		gotModel = req.Model
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer mock.Close()

	cfg := &config.Config{
		Server: config.Server{Port: 8787},
		Upstreams: []config.Upstream{
			{
				Name:         "plain-up",
				Type:         "openai",
				BaseURL:      mock.URL,
				APIKey:       "sk-test",
				Models:       []string{"client-model"},
				ModelMapping: map[string]string{"client-model": "deepseek-v4-flash"},
			},
		},
	}
	h, hc := newTestHandler(t, cfg)
	defer hc.Close()

	rr := httptest.NewRecorder()
	payload, _ := json.Marshal(map[string]string{"model": "client-model"})
	req := httptest.NewRequest(http.MethodPost, "/admin/upstreams/plain-up/test", strings.NewReader(string(payload)))
	req.SetPathValue("name", "plain-up")

	h.TestUpstream(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if gotModel != wantRealModel {
		t.Errorf("upstream received model %q, want %q", gotModel, wantRealModel)
	}
}
