package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/health"
	"github.com/icefairy/xuanji/internal/router"
	"github.com/icefairy/xuanji/internal/store"
)

// newMediaErrDetailHandler 构造指向 mock 上游的 Handler，并挂上 recorder。
// 上游模型名与客户端模型名不同（有 model_mapping），用于校验落库的 upstream_model。
func newMediaErrDetailHandler(t *testing.T, upstreamFn http.HandlerFunc) (*Handler, *store.Store) {
	t.Helper()
	upstream := httptest.NewServer(upstreamFn)
	t.Cleanup(upstream.Close)

	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{
				Name:         "media-up",
				BaseURL:      upstream.URL,
				APIKey:       "sk-media",
				Priority:     10,
				Models:       []string{"tts-1", "dall-e-3"},
				ModelMapping: map[string]string{"tts-1": "tts-1-mapped", "dall-e-3": "dall-e3-mapped"},
			},
		},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules: []config.Rule{
				{Model: "tts-1", Upstreams: []string{"media-up"}, Strategy: "primary_backup"},
				{Model: "dall-e-3", Upstreams: []string{"media-up"}, Strategy: "primary_backup"},
			},
		},
	}

	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	hc := health.New(cfg)
	t.Cleanup(hc.Close)

	h := New(cfg, router.New(cfg), hc)
	h.SetRecorder(store.NewRecorder(s))
	return h, s
}

// TestAudioSpeech_ErrorDetailRecorded 回归测试（2026-10-18 实测）：
// audio/speech 上游 502 落库后 error_detail 为空，排障时完全看不出上游说了什么
// （当天 16:26:56、16:28:42 两条 stepfun 502 均为空）。
//
// 根因：forwardMediaJSON 的 defer 写 store.Record 时**没有 ErrorDetail 字段**，
// 而 chat/rerank/embeddings 三条链路都写。这是「落库字段必须逐项对齐」家族在
// media 链路上的缺口。
func TestAudioSpeech_ErrorDetailRecorded(t *testing.T) {
	const upstreamBody = `{"error":{"message":"upstream boom detail","type":"server_error","code":"tts_fail"}}`
	h, s := newMediaErrDetailHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, upstreamBody)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech",
		strings.NewReader(`{"model":"tts-1","input":"hello","voice":"alloy"}`))
	rr := httptest.NewRecorder()
	h.AudioSpeech(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rr.Code, rr.Body.String())
	}

	rec := h.recorder
	rec.Close() // flush 落库

	var detail, upstreamModel string
	var status int
	err := s.DB().QueryRow(
		`SELECT status, COALESCE(error_detail,''), COALESCE(upstream_model,'')
		   FROM request_log WHERE endpoint='audio' ORDER BY id DESC LIMIT 1`,
	).Scan(&status, &detail, &upstreamModel)
	if err != nil {
		t.Fatalf("query request_log: %v", err)
	}

	if status != http.StatusBadGateway {
		t.Fatalf("recorded status = %d, want 502", status)
	}
	if strings.TrimSpace(detail) == "" {
		t.Fatal("error_detail 为空：上游 502 的响应体摘要必须落库，否则排障无法定位")
	}
	if !strings.Contains(detail, "upstream boom detail") {
		t.Fatalf("error_detail 未含上游错误文本: %q", detail)
	}
	if !strings.Contains(detail, "tts_fail") {
		t.Fatalf("error_detail 未含上游 error.code: %q", detail)
	}
	// 顺带校验 model 映射没有被这次改动影响
	if upstreamModel != "tts-1-mapped" {
		t.Fatalf("upstream_model = %q, want tts-1-mapped", upstreamModel)
	}
}

// TestImageGenerations_ErrorDetailRecorded 同一 defer 的第二个调用方：
// ImageGenerations 也走 forwardMediaJSON，必须一并修复（避免只改一半）。
func TestImageGenerations_ErrorDetailRecorded(t *testing.T) {
	const upstreamBody = `{"error":{"message":"image upstream boom","type":"server_error"}}`
	h, s := newMediaErrDetailHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, upstreamBody)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"dall-e-3","prompt":"a cat"}`))
	rr := httptest.NewRecorder()
	h.ImageGenerations(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rr.Code, rr.Body.String())
	}

	h.recorder.Close()

	var detail string
	err := s.DB().QueryRow(
		`SELECT COALESCE(error_detail,'') FROM request_log WHERE endpoint='images' ORDER BY id DESC LIMIT 1`,
	).Scan(&detail)
	if err != nil {
		t.Fatalf("query request_log: %v", err)
	}
	if !strings.Contains(detail, "image upstream boom") {
		t.Fatalf("error_detail 未含上游错误文本: %q", detail)
	}
}

// TestAudioSpeech_SuccessKeepsEmptyDetail 反向保护：成功请求不应写入 error_detail
// （避免把正常请求也标成有错误，污染管理页的失败原因统计）。
func TestAudioSpeech_SuccessKeepsEmptyDetail(t *testing.T) {
	h, s := newMediaErrDetailHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("fake-mp3-bytes"))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech",
		strings.NewReader(`{"model":"tts-1","input":"hello","voice":"alloy"}`))
	rr := httptest.NewRecorder()
	h.AudioSpeech(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	h.recorder.Close()

	var detail string
	var status int
	err := s.DB().QueryRow(
		`SELECT status, COALESCE(error_detail,'') FROM request_log WHERE endpoint='audio' ORDER BY id DESC LIMIT 1`,
	).Scan(&status, &detail)
	if err != nil {
		t.Fatalf("query request_log: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("recorded status = %d, want 200", status)
	}
	if detail != "" {
		t.Fatalf("成功请求的 error_detail 应为空，实际 %q", detail)
	}
}
