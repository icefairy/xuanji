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
	"github.com/icefairy/xuanji/internal/store"
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
func TestVideoQuery_ExplicitModel(t *testing.T) {
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
		// 无本地归属记录时的回退查询也应补 agnes 必需的 model_name
		// （无映射时 = 客户端模型名本身）
		if got := r.URL.Query().Get("model_name"); got != "agnes-video-v2.0" {
			t.Errorf("model_name = %q, want agnes-video-v2.0", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, upstreamBody)
	})
	defer upstream.Close()

	// 无本地记录时必须显式传 model（旧行为的静默缺省已废弃，防查错渠道）
	req := httptest.NewRequest(http.MethodGet, "/v1/videos?video_id=vid123&model=agnes-video-v2.0", nil)
	rec := httptest.NewRecorder()
	h.VideoQuery(rec, req)

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

	req := httptest.NewRequest(http.MethodGet, "/v1/videos?video_id=xxx&model=agnes-video-v2.0", nil)
	rec := httptest.NewRecorder()
	h.VideoQuery(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != upstreamBody {
		t.Errorf("body not passthrough:\n got=%s\nwant=%s", got, upstreamBody)
	}
}

// --- 创建落库 + 定向查询（agnes video_id+model_name 必带） ---

// TestVideoCreate_SavesJobAndQueryDirects 端到端：创建成功提取 video_id 落库归属
// （含映射后上游真实模型名）；随后 GET /v1/videos/{id}（OpenAI 标准路径式）
// 不传 model 即可直连承接上游，且查询 URL 自动补 model_name=<上游真实名>——
// agnes 文档要求 keyframe/reference 模式必须带该参数，纯 video_id 查不到。
func TestVideoCreate_SavesJobAndQueryDirects(t *testing.T) {
	s, err := store.Open(t.TempDir() + "/xuanji.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	var gotCreateModel, gotQueryVideoID, gotQueryModelName string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			if r.URL.Path != "/v1/videos" {
				t.Errorf("create path = %q, want /v1/videos (base_url 含 /v1)", r.URL.Path)
			}
			gotCreateModel = readBodyString(t, r)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"id":"task_1","task_id":"task_1","video_id":"video_abc","object":"video","model":"real-video-model","status":"queued","progress":0}`)
		case http.MethodGet:
			if r.URL.Path != "/agnesapi" {
				t.Errorf("query path = %q, want /agnesapi", r.URL.Path)
			}
			gotQueryVideoID = r.URL.Query().Get("video_id")
			gotQueryModelName = r.URL.Query().Get("model_name")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"id":"task_1","video_id":"video_abc","object":"video","status":"completed","progress":100,"metadata":{"url":"https://cdn.example.com/v.mp4"}}`)
		}
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Upstreams: []config.Upstream{{
			Name:         "video-up",
			BaseURL:      upstream.URL + "/v1",
			APIKey:       "sk-video",
			Priority:     10,
			Models:       []string{"agnes-video-2.5-flash"},
			ModelMapping: map[string]string{"agnes-video-2.5-flash": "real-video-model"},
		}},
		Routing: config.Routing{
			DefaultStrategy: "primary_backup",
			Rules:           []config.Rule{{Model: "agnes-video-2.5-flash", Upstreams: []string{"video-up"}, Strategy: "primary_backup"}},
		},
	}
	h := New(cfg, router.New(cfg), nil)
	h.SetRecorder(store.NewRecorder(s))

	// 1) 创建任务
	createBody := `{"model":"agnes-video-2.5-flash","prompt":"雨后街道","seconds":"5","mode":"keyframe","size":"720P","first_frame":"https://e.com/f.png"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(createBody))
	rec := httptest.NewRecorder()
	h.VideoCreate(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if gjson.Get(rec.Body.String(), "video_id").String() != "video_abc" {
		t.Fatalf("create response should passthrough video_id, got %s", rec.Body.String())
	}
	if !gjson.Get(gotCreateModel, "first_frame").Exists() || gjson.Get(gotCreateModel, "model").String() != "real-video-model" {
		t.Fatalf("upstream create body 应改写模型且保留 agnes 扩展字段 first_frame，got %s", gotCreateModel)
	}

	// 归属已落库：upstream=video-up，upstream_model=real-video-model
	job, err := s.GetVideoJob("video_abc")
	if err != nil {
		t.Fatalf("job not saved: %v", err)
	}
	if job.Upstream != "video-up" || job.UpstreamModel != "real-video-model" {
		t.Fatalf("job = %+v, want upstream=video-up upstream_model=real-video-model", job)
	}

	// 2) OpenAI 标准路径式查询，不传 model 也应定向 + 补 model_name
	qreq := httptest.NewRequest(http.MethodGet, "/v1/videos/video_abc", nil)
	qreq.SetPathValue("id", "video_abc")
	qrec := httptest.NewRecorder()
	h.VideoGetByID(qrec, qreq)
	if qrec.Code != http.StatusOK {
		t.Fatalf("query status = %d; body=%s", qrec.Code, qrec.Body.String())
	}
	if gotQueryVideoID != "video_abc" {
		t.Errorf("query video_id = %q, want video_abc", gotQueryVideoID)
	}
	if gotQueryModelName != "real-video-model" {
		t.Errorf("query model_name = %q, want real-video-model (keyframe 模式必带)", gotQueryModelName)
	}
	if !gjson.Get(qrec.Body.String(), "metadata.url").Exists() {
		t.Errorf("query response should passthrough metadata.url, got %s", qrec.Body.String())
	}

	// 3) 无记录且未传 model → 400 明确引导（不再静默缺省查错渠道）
	nreq := httptest.NewRequest(http.MethodGet, "/v1/videos?video_id=unknown", nil)
	nrec := httptest.NewRecorder()
	h.VideoQuery(nrec, nreq)
	if nrec.Code != http.StatusBadRequest {
		t.Fatalf("unknown video without model status = %d, want 400; body=%s", nrec.Code, nrec.Body.String())
	}
}

// readBodyString 读取并恢复请求体为字符串（测试辅助）。
func readBodyString(t *testing.T, r *http.Request) string {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	r.Body = io.NopCloser(strings.NewReader(string(b)))
	return string(b)
}
