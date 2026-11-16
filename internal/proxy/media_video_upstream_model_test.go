package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/router"
	"github.com/icefairy/xuanji/internal/store"
)

// 2026-09-23：媒体/视频链路此前完全没记 upstream_model（统计页显示「(未知)」），
// 本文件锁定这些端点也记录上游真实模型名。

func TestMediaAndVideo_RecordUpstreamModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/images/generations":
			fmt.Fprint(w, `{"created":1,"data":[{"url":"https://e.com/i.png"}]}`)
		case "/v1/audio/speech":
			w.Header().Set("Content-Type", "audio/mpeg")
			fmt.Fprint(w, "ID3fake")
		case "/v1/videos":
			fmt.Fprint(w, `{"id":"t1","video_id":"vid1","status":"queued"}`)
		case "/agnesapi":
			fmt.Fprint(w, `{"video_id":"vid1","status":"completed"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	rec := store.NewRecorder(s)

	cfg := &config.Config{
		Upstreams: []config.Upstream{{
			Name:     "media-up",
			BaseURL:  upstream.URL + "/v1",
			APIKey:   "sk-m",
			Priority: 10,
			Models:   []string{"dall-e-3", "tts-1", "agnes-video-2.5-flash"},
			ModelMapping: map[string]string{
				"dall-e-3":              "dall-e-3-mapped",
				"tts-1":                 "tts-1-mapped",
				"agnes-video-2.5-flash": "real-video-model",
			},
		}},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules: []config.Rule{
				{Model: "dall-e-3", Upstreams: []string{"media-up"}, Strategy: "primary_backup"},
				{Model: "tts-1", Upstreams: []string{"media-up"}, Strategy: "primary_backup"},
				{Model: "agnes-video-2.5-flash", Upstreams: []string{"media-up"}, Strategy: "primary_backup"},
			},
		},
	}
	h := New(cfg, router.New(cfg), nil)
	h.SetRecorder(rec)

	// images
	ireq := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"model":"dall-e-3","prompt":"cat"}`))
	ireq.Header.Set("Content-Type", "application/json")
	irec := httptest.NewRecorder()
	h.ImageGenerations(irec, ireq)
	if irec.Code != http.StatusOK {
		t.Fatalf("images status = %d; body=%s", irec.Code, irec.Body.String())
	}

	// audio/speech
	areq := httptest.NewRequest(http.MethodPost, "/v1/audio/speech",
		strings.NewReader(`{"model":"tts-1","input":"hi","voice":"alloy"}`))
	areq.Header.Set("Content-Type", "application/json")
	arec := httptest.NewRecorder()
	h.AudioSpeech(arec, areq)
	if arec.Code != http.StatusOK {
		t.Fatalf("audio status = %d; body=%s", arec.Code, arec.Body.String())
	}

	// videos（创建 + 查询）
	vreq := httptest.NewRequest(http.MethodPost, "/v1/videos",
		strings.NewReader(`{"model":"agnes-video-2.5-flash","prompt":"雨"}`))
	vrec := httptest.NewRecorder()
	h.VideoCreate(vrec, vreq)
	if vrec.Code != http.StatusOK {
		t.Fatalf("videos status = %d; body=%s", vrec.Code, vrec.Body.String())
	}
	qreq := httptest.NewRequest(http.MethodGet, "/v1/videos?video_id=vid1&model=agnes-video-2.5-flash", nil)
	qrec := httptest.NewRecorder()
	h.VideoQuery(qrec, qreq)
	if qrec.Code != http.StatusOK {
		t.Fatalf("video query status = %d; body=%s", qrec.Code, qrec.Body.String())
	}

	rec.Close() // flush

	// 每个 endpoint 至少一条记录，且 upstream_model 非空且为映射后真实名
	for _, tc := range []struct{ endpoint, want string }{
		{"images", "dall-e-3-mapped"},
		{"audio", "tts-1-mapped"},
		{"videos", "real-video-model"},
	} {
		got := recordedUpstreamModel(t, s, tc.endpoint)
		if got != tc.want {
			t.Errorf("%s upstream_model = %q, want %q", tc.endpoint, got, tc.want)
		}
	}
}
