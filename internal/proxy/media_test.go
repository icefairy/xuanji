package proxy

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/router"
	"github.com/tidwall/gjson"
)

// newMediaTestHandler 构造一个指向 mock 上游的 Handler，用于媒体端点测试。
func newMediaTestHandler(t *testing.T, upstreamFn http.HandlerFunc) (*httptest.Server, *Handler) {
	t.Helper()
	upstream := httptest.NewServer(upstreamFn)
	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{
				Name:         "media-up",
				BaseURL:      upstream.URL,
				APIKey:       "sk-media",
				Priority:     10,
				Models:       []string{"dall-e-3", "tts-1", "whisper-1"},
				ModelMapping: map[string]string{"dall-e-3": "dall-e-3-mapped", "tts-1": "tts-1-mapped", "whisper-1": "whisper-1-mapped"},
			},
		},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules: []config.Rule{
				{Model: "dall-e-3", Upstreams: []string{"media-up"}, Strategy: "primary_backup"},
				{Model: "tts-1", Upstreams: []string{"media-up"}, Strategy: "primary_backup"},
				{Model: "whisper-1", Upstreams: []string{"media-up"}, Strategy: "primary_backup"},
			},
		},
	}
	return upstream, New(cfg, router.New(cfg), nil)
}

// --- ImageGenerations ---

func TestImageGenerations_Passthrough(t *testing.T) {
	const upstreamBody = `{"created":123456,"data":[{"url":"https://example.com/image.png"}]}`
	upstream, h := newMediaTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-media" {
			t.Errorf("Authorization = %q, want Bearer sk-media", got)
		}
		data, _ := io.ReadAll(r.Body)
		// newMediaTestHandler 有 ModelMapping，模型名会被映射
		if got := gjson.GetBytes(data, "model").String(); got != "dall-e-3-mapped" {
			t.Errorf("received model = %q, want dall-e-3-mapped", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, upstreamBody)
	})
	defer upstream.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"dall-e-3","prompt":"a cat","n":1,"size":"1024x1024"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ImageGenerations(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != upstreamBody {
		t.Errorf("body not passthrough:\n got=%s\nwant=%s", got, upstreamBody)
	}
}

func TestImageGenerations_ModelMapping(t *testing.T) {
	upstream, h := newMediaTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		if got := gjson.GetBytes(data, "model").String(); got != "dall-e-3-mapped" {
			t.Errorf("upstream received model = %q, want dall-e-3-mapped", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"data":[{"url":"https://example.com/img.png"}]}`)
	})
	defer upstream.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"dall-e-3","prompt":"cat"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ImageGenerations(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestImageGenerations_UpstreamError(t *testing.T) {
	upstream, h := newMediaTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
	})
	defer upstream.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"dall-e-3","prompt":"cat"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ImageGenerations(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body=%s", rec.Code, rec.Body.String())
	}
	if got := gjson.Get(rec.Body.String(), "error.message").String(); got != "rate limited" {
		t.Errorf("error.message = %q, want rate limited", got)
	}
}

func TestImageGenerations_NoRoute(t *testing.T) {
	_, h := newMediaTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called for unmapped model")
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"unknown-model","prompt":"cat"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ImageGenerations(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if got := gjson.Get(rec.Body.String(), "error.code").String(); got != "model_not_found" {
		t.Errorf("error.code = %q, want model_not_found", got)
	}
}

// --- AudioSpeech ---

func TestAudioSpeech_Passthrough(t *testing.T) {
	const audioData = "fake-mp3-binary-data"
	upstream, h := newMediaTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-media" {
			t.Errorf("Authorization = %q, want Bearer sk-media", got)
		}
		data, _ := io.ReadAll(r.Body)
		// newMediaTestHandler 有 ModelMapping，模型名会被映射
		if got := gjson.GetBytes(data, "model").String(); got != "tts-1-mapped" {
			t.Errorf("received model = %q, want tts-1-mapped", got)
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, audioData)
	})
	defer upstream.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech",
		strings.NewReader(`{"model":"tts-1","input":"Hello world","voice":"alloy"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.AudioSpeech(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "audio/mpeg" {
		t.Errorf("Content-Type = %q, want audio/mpeg", got)
	}
	if got := rec.Body.String(); got != audioData {
		t.Errorf("body not passthrough:\n got=%s\nwant=%s", got, audioData)
	}
}

func TestAudioSpeech_ModelMapping(t *testing.T) {
	upstream, h := newMediaTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		if got := gjson.GetBytes(data, "model").String(); got != "tts-1-mapped" {
			t.Errorf("upstream received model = %q, want tts-1-mapped", got)
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "audio-data")
	})
	defer upstream.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech",
		strings.NewReader(`{"model":"tts-1","input":"Hello"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.AudioSpeech(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestAudioSpeech_UpstreamError(t *testing.T) {
	upstream, h := newMediaTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `internal error`)
	})
	defer upstream.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech",
		strings.NewReader(`{"model":"tts-1","input":"Hello"}`))
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

// --- AudioTranscriptions ---

// buildTranscriptionMultipart 构造 multipart 请求体，用于音频转录测试。
func buildTranscriptionMultipart(t *testing.T, model, audioContent string) ([]byte, string, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	// 模拟 file 字段
	hdr := make(textproto.MIMEHeader)
	hdr.Set("Content-Disposition", `form-data; name="file"; filename="test.mp3"`)
	hdr.Set("Content-Type", "audio/mpeg")
	fw, err := mw.CreatePart(hdr)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(fw, audioContent)

	mw.WriteField("model", model)
	mw.Close()
	return buf.Bytes(), mw.FormDataContentType(), mw.Boundary()
}

func TestAudioTranscriptions_Passthrough(t *testing.T) {
	const transcriptResp = `{"text":"hello world"}`
	upstream, h := newMediaTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-media" {
			t.Errorf("Authorization = %q, want Bearer sk-media", got)
		}
		// 验证 multipart 中有 model 字段
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			t.Fatal(err)
		}
		// newMediaTestHandler 有 ModelMapping，模型名会被映射
		if got := r.FormValue("model"); got != "whisper-1-mapped" {
			t.Errorf("model = %q, want whisper-1-mapped", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, transcriptResp)
	})
	defer upstream.Close()

	bodyBytes, contentType, _ := buildTranscriptionMultipart(t, "whisper-1", "fake-audio-data")
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	h.AudioTranscriptions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != transcriptResp {
		t.Errorf("body not passthrough:\n got=%s\nwant=%s", got, transcriptResp)
	}
}

func TestAudioTranscriptions_ModelMapping(t *testing.T) {
	upstream, h := newMediaTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			t.Fatal(err)
		}
		if got := r.FormValue("model"); got != "whisper-1-mapped" {
			t.Errorf("upstream received model = %q, want whisper-1-mapped", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"text":"mapped"}`)
	})
	defer upstream.Close()

	bodyBytes, contentType, _ := buildTranscriptionMultipart(t, "whisper-1", "audio-data")
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	h.AudioTranscriptions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestAudioTranscriptions_UpstreamError(t *testing.T) {
	upstream, h := newMediaTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"invalid audio format","type":"invalid_request_error"}}`)
	})
	defer upstream.Close()

	bodyBytes, contentType, _ := buildTranscriptionMultipart(t, "whisper-1", "bad-audio")
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	h.AudioTranscriptions(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if got := gjson.Get(rec.Body.String(), "error.message").String(); got != "invalid audio format" {
		t.Errorf("error.message = %q, want invalid audio format", got)
	}
}
