package admin

import (
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/icefairy/xuanji/internal/store"
)

// TestToggleUpstream 验证启用/禁用切换写库并生效。
func TestToggleUpstream(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateUpstream(&store.UpstreamRow{Name: "测试上游", BaseURL: "http://x.local", Models: "[]", Enabled: 1}); err != nil {
		t.Fatal(err)
	}

	h := New(testConfig(), nil)
	h.SetStore(s)
	reloaded := 0
	h.SetReload(func() error { reloaded++; return nil })

	// 第一次 toggle → 禁用
	req1 := httptest.NewRequest("PUT", "/admin/upstreams/test/toggle", nil)
	req1.SetPathValue("name", "测试上游")
	rr := httptest.NewRecorder()
	h.ToggleUpstream(rr, req1)
	up, _ := s.GetUpstream("测试上游")
	if up.Enabled != 0 {
		t.Errorf("after first toggle enabled = %d, want 0 (禁用)", up.Enabled)
	}
	if reloaded != 1 {
		t.Errorf("reload called %d times, want 1", reloaded)
	}

	// 第二次 toggle → 启用
	req2 := httptest.NewRequest("PUT", "/admin/upstreams/test/toggle", nil)
	req2.SetPathValue("name", "测试上游")
	h.ToggleUpstream(httptest.NewRecorder(), req2)
	up, _ = s.GetUpstream("测试上游")
	if up.Enabled != 1 {
		t.Errorf("after second toggle enabled = %d, want 1 (启用)", up.Enabled)
	}
}

func TestFmtCST(t *testing.T) {
	cases := []struct{ in, want string }{
		{"2026-08-01T15:23:36Z", "2026-08-01 23:23:36"},
		{"2026-01-01T00:00:00Z", "2026-01-01 08:00:00"},
		{"bad-time", "bad-time"},
	}
	for _, c := range cases {
		if got := fmtCST(c.in); got != c.want {
			t.Errorf("fmtCST(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRequestLogs_FilterAndPaging(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// 3 条 上游A/M1，2 条 上游B/M2
	for i := 0; i < 3; i++ {
		if err := s.Insert(store.Record{Timestamp: time.Now(), Upstream: "上游A", Model: "M1", Status: 200, DurationMS: 1, Tokens: 10}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := s.Insert(store.Record{Timestamp: time.Now(), Upstream: "上游B", Model: "M2", Status: 500, DurationMS: 2, Tokens: 20}); err != nil {
			t.Fatal(err)
		}
	}

	h := New(testConfig(), nil)
	h.SetStore(s)

	type logResp struct {
		Total int64 `json:"total"`
		Logs  []struct {
			Ts string `json:"ts"`
		} `json:"logs"`
		Filters struct {
			Upstreams []string `json:"upstreams"`
			Models    []string `json:"models"`
		} `json:"filters"`
	}

	// 全量 + 分页 limit=2
	rr := httptest.NewRecorder()
	h.RequestLogs(rr, httptest.NewRequest("GET", "/admin/logs?limit=2&offset=0", nil))
	var out logResp
	decodeBody(t, rr, &out)
	if out.Total != 5 {
		t.Errorf("total = %d, want 5", out.Total)
	}
	if len(out.Logs) != 2 {
		t.Errorf("logs len = %d, want 2 (limit)", len(out.Logs))
	}
	if len(out.Filters.Upstreams) != 2 || len(out.Filters.Models) != 2 {
		t.Errorf("filters = %+v, want 2 upstreams + 2 models", out.Filters)
	}
	if ts := out.Logs[0].Ts; len(ts) != 19 {
		t.Errorf("ts = %q, want 19 chars (YYYY-MM-DD HH:MM:SS)", ts)
	}

	// 按上游筛选（中文需编码）
	rr2 := httptest.NewRecorder()
	h.RequestLogs(rr2, httptest.NewRequest("GET", "/admin/logs?upstream="+url.QueryEscape("上游A"), nil))
	var out2 logResp
	decodeBody(t, rr2, &out2)
	if out2.Total != 3 {
		t.Errorf("filter upstream=上游A total = %d, want 3", out2.Total)
	}

	// 按模型筛选
	rr3 := httptest.NewRecorder()
	h.RequestLogs(rr3, httptest.NewRequest("GET", "/admin/logs?model=M2", nil))
	var out3 logResp
	decodeBody(t, rr3, &out3)
	if out3.Total != 2 {
		t.Errorf("filter model=M2 total = %d, want 2", out3.Total)
	}

	// 组合筛选（上游A + 模型M2 → 0 条）
	rr4 := httptest.NewRecorder()
	h.RequestLogs(rr4, httptest.NewRequest("GET", "/admin/logs?upstream="+url.QueryEscape("上游A")+"&model=M2", nil))
	var out4 logResp
	decodeBody(t, rr4, &out4)
	if out4.Total != 0 {
		t.Errorf("combined filter total = %d, want 0", out4.Total)
	}
}

// TestRequestLogs_ProfileMap 验证 /admin/logs 响应带 profile_map
// （client_addr → 已识别程序；program 为空或'未知'的不进映射）。
func TestRequestLogs_ProfileMap(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// addr1 → Hermes（进映射）；addr2 → 未知（不进映射）
	for _, p := range []store.ClientProfile{
		{ClientAddr: "127.0.0.1:52001", Program: "Hermes", Confidence: 0.95, Evidence: "UA=Hermes/0.5.2"},
		{ClientAddr: "127.0.0.1:52002", Program: "未知", Confidence: 0.1, Evidence: "UA 未识别"},
	} {
		if err := s.UpsertClientProfile(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Insert(store.Record{Timestamp: time.Now(), Upstream: "up", Model: "m", Endpoint: "chat", Status: 200, ClientAddr: "127.0.0.1:52001"}); err != nil {
		t.Fatal(err)
	}

	h := New(testConfig(), nil)
	h.SetStore(s)

	rr := httptest.NewRecorder()
	h.RequestLogs(rr, httptest.NewRequest("GET", "/admin/logs", nil))
	var out struct {
		Logs       []map[string]interface{} `json:"logs"`
		ProfileMap map[string]struct {
			Program    string  `json:"program"`
			Confidence float64 `json:"confidence"`
		} `json:"profile_map"`
	}
	decodeBody(t, rr, &out)
	if out.ProfileMap == nil {
		t.Fatal("profile_map 缺失")
	}
	if p, ok := out.ProfileMap["127.0.0.1:52001"]; !ok || p.Program != "Hermes" || p.Confidence != 0.95 {
		t.Errorf("profile_map[52001] = %+v, want Hermes/0.95", p)
	}
	if _, ok := out.ProfileMap["127.0.0.1:52002"]; ok {
		t.Errorf("profile_map 不应包含 '未知' 程序地址")
	}
	if len(out.Logs) != 1 {
		t.Errorf("logs len = %d, want 1", len(out.Logs))
	}
}
