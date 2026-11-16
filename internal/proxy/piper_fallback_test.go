package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/router"
)

// 本地兜底上游：piper 走标准 OpenAI /v1/audio/speech，不能命中 mimo 的 chat 分支。
func TestAudioSpeech_FallbackToLocalPiper(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	var gotVoice string

	piper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		var req map[string]any
		_ = json.Unmarshal(b, &req)
		if v, ok := req["voice"].(string); ok {
			gotVoice = v
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("FAKE-MP3-BYTES"))
	}))
	defer piper.Close()

	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{
				Name:     "piper-local",
				BaseURL:  piper.URL + "/v1",
				APIKey:   "sk-local",
				Priority: 99,
				Kind:     "tts",
				Models:   []string{"mimo-v2.5-tts"},
			},
		},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules: []config.Rule{
				{Model: "mimo-v2.5-tts", Upstreams: []string{"piper-local"}, Strategy: "primary_backup"},
			},
		},
	}
	h := New(cfg, router.New(cfg), nil)

	body := `{"model":"mimo-v2.5-tts","input":"兜底测试","voice":"cn_female_shasha","response_format":"mp3"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.AudioSpeech(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/audio/speech" {
		t.Errorf("piper path = %q, want /v1/audio/speech (must NOT be wrapped as chat)", gotPath)
	}
	if gotAuth != "Bearer sk-local" {
		t.Errorf("Authorization = %q, want Bearer sk-local", gotAuth)
	}
	// 关键：请求体必须原样透传 OpenAI 契约，不能被改成 mimo 的 messages/audio 结构
	if strings.Contains(gotBody, `"messages"`) {
		t.Errorf("body was rewritten into mimo chat format: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"input"`) {
		t.Errorf("body lost OpenAI 'input' field: %s", gotBody)
	}
	if gotVoice != "cn_female_shasha" {
		t.Errorf("voice = %q, want passthrough cn_female_shasha", gotVoice)
	}
	if rec.Body.String() != "FAKE-MP3-BYTES" {
		t.Errorf("audio bytes not relayed, got %q", rec.Body.String())
	}
}

// 真正的 mimo 上游仍必须走 chat 分支（不能因为加了兜底判断而回归）。
func TestAudioSpeech_MimoStillUsesChatBranch(t *testing.T) {
	var gotPath string
	var gotBody string

	mimo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"audio":{"data":"` +
			strings.Repeat("A", 8) + `"}}}]}`))
	}))
	defer mimo.Close()

	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{Name: "mimo", BaseURL: mimo.URL + "/v1", APIKey: "sk-mimo", Priority: 10,
				Kind: "tts", Models: []string{"mimo-v2.5-tts"}},
		},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules: []config.Rule{
				{Model: "mimo-v2.5-tts", Upstreams: []string{"mimo"}, Strategy: "primary_backup"},
			},
		},
	}
	h := New(cfg, router.New(cfg), nil)

	body := `{"model":"mimo-v2.5-tts","input":"你好","voice":"冰糖"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.AudioSpeech(rec, req)

	if gotPath != "/v1/chat/completions" {
		t.Errorf("mimo path = %q, want /v1/chat/completions", gotPath)
	}
	if !strings.Contains(gotBody, `"messages"`) {
		t.Errorf("mimo body should be chat format, got %s", gotBody)
	}
}
