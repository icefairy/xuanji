package proxy

import (
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

// 含 video_url 的多模态请求体
const videoBody = `{"model":"qwen3.6:35b","messages":[{"role":"user","content":[{"type":"text","text":"分析这个视频"},{"type":"video_url","video_url":{"url":"https://example.com/video.mp4"}}]}]}`

// 纯文本请求体
const textBody = `{"model":"qwen3.6:35b","messages":[{"role":"user","content":"你好"}]}`

// 含 image_url 但无 video_url（应放行）
const imageBody = `{"model":"qwen3.6:35b","messages":[{"role":"user","content":[{"type":"text","text":"看看图"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`

func TestContainsVideoURL(t *testing.T) {
	if !containsVideoURL([]byte(videoBody)) {
		t.Fatal("videoBody 应检测到 video_url")
	}
	if containsVideoURL([]byte(textBody)) {
		t.Fatal("textBody 不应检测到 video_url")
	}
	if containsVideoURL([]byte(imageBody)) {
		t.Fatal("imageBody 不应检测到 video_url")
	}
	if containsVideoURL([]byte(`not json`)) {
		t.Fatal("非法 JSON 应返回 false")
	}
}

// 开关默认关闭（安全默认：视频流量大，需显式开启）。
func TestVideoPassThroughDefaultOff(t *testing.T) {
	h := &Handler{cfg: &config.Config{}}
	if h.cfg.Proxy.VideoPassThrough {
		t.Fatal("默认应关闭视频透传")
	}
}

// --- 视频生成 /v1/videos ---

// newVideoTestHandler 构造一个指向 mock 上游的 Handler，用于视频接口测试。
// baseURLSuffix 可追加到上游 URL 末尾（如 "/v1"），模拟 agnes 的 base_url 形态。
func newVideoTestHandler(t *testing.T, baseURLSuffix string, upstreamFn http.HandlerFunc) (*httptest.Server, *Handler) {
	t.Helper()
	upstream := httptest.NewServer(upstreamFn)
	cfg := &config.Config{
		Upstreams: []config.Upstream{
			{
				Name:     "video-up",
				BaseURL:  upstream.URL + baseURLSuffix,
				APIKey:   "sk-video",
				Priority: 10,
				Models:   []string{"agnes-video-v2.0"},
			},
		},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules: []config.Rule{
				{Model: "agnes-video-v2.0", Upstreams: []string{"video-up"}, Strategy: "primary_backup"},
			},
		},
	}
	return upstream, New(cfg, router.New(cfg), nil)
}

func TestVideoCreate_NoModel(t *testing.T) {
	_, h := newVideoTestHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called without model")
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/videos",
		strings.NewReader(`{"prompt":"a cat walking"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.VideoCreate(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if got := gjson.Get(rec.Body.String(), "error.message").String(); got != "model is required" {
		t.Errorf("error.message = %q, want model is required", got)
	}
}

func TestVideoCreate_NoRoute(t *testing.T) {
	_, h := newVideoTestHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called for unmapped model")
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/videos",
		strings.NewReader(`{"model":"unknown-video-model","prompt":"a cat"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.VideoCreate(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if got := gjson.Get(rec.Body.String(), "error.code").String(); got != "model_not_found" {
		t.Errorf("error.code = %q, want model_not_found", got)
	}
}

func TestVideoCreate_Passthrough(t *testing.T) {
	const upstreamBody = `{"video_id":"abc","status":"submitted"}`
	upstream, h := newVideoTestHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/videos" {
			t.Errorf("path = %q, want /videos", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-video" {
			t.Errorf("Authorization = %q, want Bearer sk-video", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		data, _ := io.ReadAll(r.Body)
		if got := gjson.GetBytes(data, "model").String(); got != "agnes-video-v2.0" {
			t.Errorf("received model = %q, want agnes-video-v2.0", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, upstreamBody)
	})
	defer upstream.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/videos",
		strings.NewReader(`{"model":"agnes-video-v2.0","prompt":"a cat walking","mode":"ti2vid","width":1280,"height":720,"num_frames":97,"frame_rate":24}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.VideoCreate(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != upstreamBody {
		t.Errorf("body not passthrough:\n got=%s\nwant=%s", got, upstreamBody)
	}
}

// --- 视频状态查询 GET /v1/videos?video_id=xxx ---

func TestVideoQuery_NoVideoID(t *testing.T) {
	_, h := newVideoTestHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called without video_id")
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/videos", nil)
	rec := httptest.NewRecorder()
	h.VideoQuery(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if got := gjson.Get(rec.Body.String(), "error.message").String(); got != "video_id is required" {
		t.Errorf("error.message = %q, want video_id is required", got)
	}
}

// VideoQuery 无 model 参数时默认 agnes-video-v2.0（走 routing_rules 轮换池），
// 转发到 agnes 查询端点 /agnesapi。
func TestVideoQuery_DefaultModel(t *testing.T) {
	const upstreamBody = `{"status":"processing","url":""}`
	upstream, h := newVideoTestHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/agnesapi" {
			t.Errorf("path = %q, want /agnesapi", r.URL.Path)
		}
		if got := r.URL.Query().Get("video_id"); got != "vid123" {
			t.Errorf("video_id = %q, want vid123", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, upstreamBody)
	})
	defer upstream.Close()

	req := httptest.NewRequest(http.MethodGet, "/v1/videos?video_id=vid123", nil)
	rec := httptest.NewRecorder()
	h.VideoQuery(rec, req)

	// 默认 model 路由成功才会到达上游；若默认值失效会 404
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != upstreamBody {
		t.Errorf("body not passthrough:\n got=%s\nwant=%s", got, upstreamBody)
	}
}

// forwardVideoQuery 的 URL 构造：base_url 末尾带 /v1 时必须去掉再拼 /agnesapi。
func TestVideoQuery_StripsV1FromBaseURL(t *testing.T) {
	const upstreamBody = `{"status":"done","url":"https://example.com/out.mp4"}`
	upstream, h := newVideoTestHandler(t, "/v1", func(w http.ResponseWriter, r *http.Request) {
		// 断言去掉了 /v1：路径必须是 /agnesapi 而非 /v1/agnesapi
		if r.URL.Path != "/agnesapi" {
			t.Errorf("path = %q, want /agnesapi (base_url 的 /v1 应被去掉)", r.URL.Path)
		}
		if got := r.URL.Query().Get("video_id"); got != "xxx" {
			t.Errorf("video_id = %q, want xxx", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, upstreamBody)
	})
	defer upstream.Close()

	req := httptest.NewRequest(http.MethodGet, "/v1/videos?video_id=xxx", nil)
	rec := httptest.NewRecorder()
	h.VideoQuery(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != upstreamBody {
		t.Errorf("body not passthrough:\n got=%s\nwant=%s", got, upstreamBody)
	}
}
