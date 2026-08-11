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
	// 3 条 上游A/M1，2 条 上游B/M2（其中 1 条 endpoint=completions，模拟老接口调用）
	for i := 0; i < 3; i++ {
		if err := s.Insert(store.Record{Timestamp: time.Now(), Upstream: "上游A", Model: "M1", Status: 200, DurationMS: 1, Tokens: 10, Endpoint: "chat"}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := s.Insert(store.Record{Timestamp: time.Now(), Upstream: "上游B", Model: "M2", Status: 500, DurationMS: 2, Tokens: 20, Endpoint: "completions"}); err != nil {
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
			Endpoints []string `json:"endpoints"`
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
	if len(out.Filters.Upstreams) != 2 || len(out.Filters.Models) != 2 || len(out.Filters.Endpoints) != 2 {
		t.Errorf("filters = %+v, want 2 upstreams + 2 models + 2 endpoints", out.Filters)
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

	// 状态筛选：正常 (2xx) → 上游A 的 3 条；异常 (非2xx) → 上游B 的 2 条
	rr5 := httptest.NewRecorder()
	h.RequestLogs(rr5, httptest.NewRequest("GET", "/admin/logs?status_type=normal", nil))
	var out5 logResp
	decodeBody(t, rr5, &out5)
	if out5.Total != 3 {
		t.Errorf("status_type=normal total = %d, want 3", out5.Total)
	}

	rr6 := httptest.NewRecorder()
	h.RequestLogs(rr6, httptest.NewRequest("GET", "/admin/logs?status_type=error", nil))
	var out6 logResp
	decodeBody(t, rr6, &out6)
	if out6.Total != 2 {
		t.Errorf("status_type=error total = %d, want 2", out6.Total)
	}

	// 端点筛选：endpoint=completions → 上游B 的 2 条
	rr8 := httptest.NewRecorder()
	h.RequestLogs(rr8, httptest.NewRequest("GET", "/admin/logs?endpoint=completions", nil))
	var out8 logResp
	decodeBody(t, rr8, &out8)
	if out8.Total != 2 {
		t.Errorf("filter endpoint=completions total = %d, want 2", out8.Total)
	}

	// 端点 + 状态组合：endpoint=completions + 异常 → 2 条（都是 500）
	rr9 := httptest.NewRecorder()
	h.RequestLogs(rr9, httptest.NewRequest("GET", "/admin/logs?endpoint=completions&status_type=error", nil))
	var out9 logResp
	decodeBody(t, rr9, &out9)
	if out9.Total != 2 {
		t.Errorf("filter endpoint=completions+error total = %d, want 2", out9.Total)
	}
}
