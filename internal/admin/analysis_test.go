package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/store"
)

// TestParsePiJSON 验证 pi 输出的宽松 JSON 提取：
// 纯 JSON / Markdown 代码块 / 带前后缀文字 都应解析成功。
func TestParsePiJSON(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		program string
		conf    float64
		evid    string
		wantErr bool
	}{
		{name: "plain", in: `{"program":"Hermes","confidence":0.8,"evidence":"UA=python-requests"}`,
			program: "Hermes", conf: 0.8, evid: "UA=python-requests"},
		{name: "code fence", in: "```json\n{\"program\":\"Claude Code\",\"confidence\":0.9,\"evidence\":\"UA=claude-cli\"}\n```",
			program: "Claude Code", conf: 0.9, evid: "UA=claude-cli"},
		{name: "prefix text", in: "好的，分析结果如下：\n{\"program\":\"OpenCode\",\"confidence\":0.7,\"evidence\":\"端口特征\"}\n希望对你有帮助",
			program: "OpenCode", conf: 0.7, evid: "端口特征"},
		{name: "confidence clamp high", in: `{"program":"pi","confidence":1.5,"evidence":"x"}`,
			program: "pi", conf: 1.0, evid: "x"},
		{name: "empty program", in: `{"program":"","confidence":0.1,"evidence":""}`,
			program: "未知", conf: 0.1},
		{name: "no json", in: "我不确定", wantErr: true},
		{name: "invalid json", in: `{"program":`, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			program, conf, evid, err := parsePiJSON([]byte(c.in))
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got program=%q conf=%v", program, conf)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePiJSON: %v", err)
			}
			if program != c.program || conf != c.conf || evid != c.evid {
				t.Errorf("got (%q, %v, %q), want (%q, %v, %q)", program, conf, evid, c.program, c.conf, c.evid)
			}
		})
	}
}

// TestIsLocalAddr 验证本机 IP 过滤。
func TestIsLocalAddr(t *testing.T) {
	locals := localIPs()
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:53211", true},
		{"::1:53211", true},
		{"0.0.0.0:80", true},
		{"localhost:8080", true},
		{"192.168.1.10:53211", true}, // 网关自身内网 IP（任务文档明确要求过滤）
		{"192.168.1.20:53211", false},
		{"10.0.0.5:443", false},
	}
	for _, c := range cases {
		if got := isLocalAddr(c.addr, locals); got != c.want {
			t.Errorf("isLocalAddr(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

// testClientAnalysisConfig 构造开启客户端分析的配置。
func testClientAnalysisConfig() *config.Config {
	cfg := testConfig()
	cfg.Proxy.ClientAnalysis = true
	cfg.Proxy.ClientAnalysisInterval = 600
	return cfg
}

// TestAnalyzer_RunOnce 端到端验证分析流程：
// 聚合去重 client_addr → 过滤本机 IP → pi 推断 → upsert 落库。
func TestAnalyzer_RunOnce(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()

	now := time.Now().UTC()
	// 两条外部地址（192.168.1.20 出现 2 次，192.168.1.21 出现 1 次）+ 一条本机地址（应被过滤）
	recs := []store.Record{
		{Timestamp: now, Upstream: "up", Model: "deepseek-v4-flash", Endpoint: "chat", Status: 200, ClientAddr: "192.168.1.20:53211"},
		{Timestamp: now, Upstream: "up", Model: "deepseek-v4-flash", Endpoint: "chat", Status: 200, ClientAddr: "192.168.1.20:53211"},
		{Timestamp: now, Upstream: "up", Model: "gpt-5", Endpoint: "chat", Status: 200, ClientAddr: "192.168.1.21:6000"},
		{Timestamp: now, Upstream: "up", Model: "deepseek-v4-flash", Endpoint: "chat", Status: 200, ClientAddr: "127.0.0.1:5555"},
	}
	if err := s.InsertBatch(recs); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	cfg := testClientAnalysisConfig()
	a := NewClientAnalyzer(cfg, s)
	// 注入假 pi：按 addr 返回不同结果
	a.piRunner = func(addr string, feat store.ClientAddrFeature) (string, float64, string, error) {
		if strings.HasPrefix(addr, "192.168.1.20") {
			return "Hermes", 0.8, "UA=python-requests, 端口53211", nil
		}
		return "OpenCode", 0.7, "UA=opencode", nil
	}

	resp, err := a.RunOnce(true)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("status = %q, want ok", resp.Status)
	}
	if resp.Analyzed != 2 {
		t.Errorf("analyzed = %d, want 2（本机 127.0.0.1 应被过滤）", resp.Analyzed)
	}
	if resp.NewProfiles != 2 {
		t.Errorf("new_profiles = %d, want 2", resp.NewProfiles)
	}

	// 落库检查
	profiles, err := s.ListClientProfiles()
	if err != nil {
		t.Fatalf("ListClientProfiles: %v", err)
	}
	if len(profiles) != 2 {
		t.Fatalf("profiles count = %d, want 2", len(profiles))
	}
	byAddr := map[string]store.ClientProfile{}
	for _, p := range profiles {
		byAddr[p.ClientAddr] = p
	}
	if p := byAddr["192.168.1.20:53211"]; p.Program != "Hermes" || p.Confidence != 0.8 {
		t.Errorf("192.168.1.20 profile = %+v, want Hermes/0.8", p)
	}
	if p := byAddr["192.168.1.21:6000"]; p.Program != "OpenCode" || p.Confidence != 0.7 {
		t.Errorf("192.168.1.21 profile = %+v, want OpenCode/0.7", p)
	}
	// 本机地址不应出现在档案里
	if _, ok := byAddr["127.0.0.1:5555"]; ok {
		t.Error("127.0.0.1:5555 不应出现在 profiles（本机过滤失败）")
	}

	// 再次运行：重复分析应 upsert 更新而非新增（行数不变）
	if _, err := a.RunOnce(true); err != nil {
		t.Fatalf("RunOnce second: %v", err)
	}
	profiles2, _ := s.ListClientProfiles()
	if len(profiles2) != 2 {
		t.Errorf("profiles after second run = %d, want 2（upsert 去重）", len(profiles2))
	}
}

// TestAnalyzer_Disabled 验证开关关闭时手动触发被拒绝。
func TestAnalyzer_Disabled(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()

	cfg := testConfig() // ClientAnalysis 默认 false
	a := NewClientAnalyzer(cfg, s)
	a.piRunner = func(addr string, feat store.ClientAddrFeature) (string, float64, string, error) {
		return "x", 0.5, "", nil
	}
	if _, err := a.RunOnce(true); err == nil {
		t.Fatal("RunOnce 应在开关关闭时返回错误")
	}
}

// TestAnalysisProfiles_Endpoint 验证 GET /admin/analysis/profiles 端点返回档案列表。
func TestAnalysisProfiles_Endpoint(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()
	if err := s.UpsertClientProfile(store.ClientProfile{
		ClientAddr: "192.168.1.20:53211",
		Program:    "Hermes",
		Confidence: 0.8,
		Evidence:   "UA=python-requests",
	}); err != nil {
		t.Fatalf("UpsertClientProfile: %v", err)
	}

	h, hc := newTestHandler(t, testConfig())
	defer hc.Close()
	h.SetStore(s)

	rr := httptest.NewRecorder()
	h.AnalysisProfiles(rr, httptest.NewRequest(http.MethodGet, "/admin/analysis/profiles", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}
	var out []store.ClientProfile
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 || out[0].Program != "Hermes" {
		t.Errorf("profiles = %+v, want 1×Hermes", out)
	}
}

// TestAnalysisRun_Endpoint 验证 POST /admin/analysis/run 手动触发端点。
func TestAnalysisRun_Endpoint(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()
	if err := s.Insert(store.Record{
		Timestamp: time.Now().UTC(), Upstream: "up", Model: "m",
		Endpoint: "chat", Status: 200, ClientAddr: "10.0.0.8:9999",
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	h, hc := newTestHandler(t, testClientAnalysisConfig())
	defer hc.Close()
	h.SetStore(s)

	a := NewClientAnalyzer(testClientAnalysisConfig(), s)
	a.piRunner = func(addr string, feat store.ClientAddrFeature) (string, float64, string, error) {
		return "Cherry Studio", 0.6, "UA=cherry", nil
	}
	h.SetAnalyzer(a)

	rr := httptest.NewRecorder()
	h.AnalysisRun(rr, httptest.NewRequest(http.MethodPost, "/admin/analysis/run", nil))
	var out analysisRunResponse
	decodeBody(t, rr, &out)
	if out.Status != "ok" || out.Analyzed != 1 || out.NewProfiles != 1 {
		t.Errorf("response = %+v, want status=ok analyzed=1 new_profiles=1", out)
	}
}
