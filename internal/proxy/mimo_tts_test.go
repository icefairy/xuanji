package proxy

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/router"
	"github.com/tidwall/gjson"
)

// mimoAudioResp 构造 MiMo chat/completions 响应，data 为 base64 音频。
func mimoAudioResp(t *testing.T, audio string) string {
	t.Helper()
	return fmt.Sprintf(`{"choices":[{"message":{"audio":{"data":%q}}}]}`,
		base64.StdEncoding.EncodeToString([]byte(audio)))
}

// newMimoTestHandler 构造一个指向 mock 上游的 Handler，用于 mimo TTS 桥接测试。
func newMimoTestHandler(t *testing.T, upstreamFn http.HandlerFunc) (*httptest.Server, *Handler) {
	t.Helper()
	upstream := httptest.NewServer(upstreamFn)
	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{
				Name:     "mimo-up",
				BaseURL:  upstream.URL + "/v1",
				APIKey:   "sk-mimo",
				Priority: 10,
				Models:   []string{"mimo-v2.5-tts", "mimo-v2.5-tts-voiceclone", "mimo-v2.5-tts-voicedesign"},
			},
		},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules: []config.Rule{
				{Model: "mimo-v2.5-tts*", Upstreams: []string{"mimo-up"}, Strategy: "primary_backup"},
			},
		},
	}
	return upstream, New(cfg, router.New(cfg), nil)
}

// --- AudioSpeech 的 MiMo 桥接 ---

func TestMimoTTS_ConvertAndDecode(t *testing.T) {
	upstream, h := newMimoTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/chat/completions" {
			t.Errorf("request path = %q, want /v1/chat/completions", got)
		}
		data, _ := io.ReadAll(r.Body)
		if got := gjson.GetBytes(data, "model").String(); got != "mimo-v2.5-tts" {
			t.Errorf("model = %q, want mimo-v2.5-tts", got)
		}
		// 消息必须同时包含 user 与 assistant
		msgs := gjson.GetBytes(data, "messages").Array()
		if len(msgs) != 2 {
			t.Fatalf("messages count = %d, want 2", len(msgs))
		}
		if got := msgs[0].Get("role").String(); got != "user" {
			t.Errorf("messages[0].role = %q, want user", got)
		}
		if got := msgs[0].Get("content").String(); got != "你好，这是小米的免费语音合成测试" {
			t.Errorf("messages[0].content = %q", got)
		}
		if got := msgs[1].Get("role").String(); got != "assistant" {
			t.Errorf("messages[1].role = %q, want assistant", got)
		}
		// 未传 voice 时默认 mimo_default
		if got := gjson.GetBytes(data, "audio.voice").String(); got != "mimo_default" {
			t.Errorf("audio.voice = %q, want mimo_default", got)
		}
		if got := gjson.GetBytes(data, "audio.format").String(); got != "mpeg" {
			t.Errorf("audio.format = %q, want mpeg", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, mimoAudioResp(t, "MOCK-AUDIO"))
	})
	defer upstream.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech",
		strings.NewReader(`{"model":"mimo-v2.5-tts","input":"你好，这是小米的免费语音合成测试","response_format":"mpeg"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.AudioSpeech(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "audio/mpeg" {
		t.Errorf("Content-Type = %q, want audio/mpeg", got)
	}
	if got := rec.Body.String(); got != "MOCK-AUDIO" {
		t.Errorf("body = %q, want MOCK-AUDIO", got)
	}
}

func TestMimoTTS_VoiceMapping(t *testing.T) {
	upstream, h := newMimoTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		// OpenAI 标准音色 alloy 应映射为 mimo_default
		if got := gjson.GetBytes(data, "audio.voice").String(); got != "mimo_default" {
			t.Errorf("audio.voice = %q, want mimo_default", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, mimoAudioResp(t, "MOCK-AUDIO"))
	})
	defer upstream.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech",
		strings.NewReader(`{"model":"mimo-v2.5-tts","input":"Hello","voice":"alloy"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.AudioSpeech(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestMimoTTS_NoSystemRole(t *testing.T) {
	upstream, h := newMimoTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		for _, m := range gjson.GetBytes(data, "messages").Array() {
			if got := m.Get("role").String(); got == "system" {
				t.Errorf("messages contains forbidden system role")
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, mimoAudioResp(t, "MOCK-AUDIO"))
	})
	defer upstream.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech",
		strings.NewReader(`{"model":"mimo-v2.5-tts","input":"Hello","voice":"冰糖"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.AudioSpeech(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestMimoTTS_AuthHeader(t *testing.T) {
	upstream, h := newMimoTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("api-key"); got != "sk-mimo" {
			t.Errorf("api-key header = %q, want sk-mimo", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization header = %q, want empty (must use api-key)", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, mimoAudioResp(t, "MOCK-AUDIO"))
	})
	defer upstream.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech",
		strings.NewReader(`{"model":"mimo-v2.5-tts","input":"Hello"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.AudioSpeech(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestMimoTTS_UpstreamError(t *testing.T) {
	upstream, h := newMimoTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `internal error`)
	})
	defer upstream.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech",
		strings.NewReader(`{"model":"mimo-v2.5-tts","input":"Hello"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.AudioSpeech(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if got := gjson.Get(rec.Body.String(), "error.type").String(); got != "server_error" {
		t.Errorf("error.type = %q, want server_error", got)
	}
}
