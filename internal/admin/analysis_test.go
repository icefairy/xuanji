package admin

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/store"
)

// TestIsMeaninglessAddr 验证无意义地址过滤：只排除空串/0.0.0.0/::，
// 本机地址（127.0.0.1 / ::1 / 本机网卡 IP）不再被过滤。
func TestIsMeaninglessAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"", true},
		{"0.0.0.0:80", true},
		{"::", true},
		{"[::]:80", true},
		{"127.0.0.1:53211", false}, // 本机地址保留（按端口查进程）
		{"::1:53211", false},
		{"localhost:8080", false},
		{"192.168.1.10:53211", false}, // 本机网卡 IP 保留
		{"192.168.1.20:53211", false},
		{"10.0.0.5:443", false},
	}
	for _, c := range cases {
		if got := isMeaninglessAddr(c.addr); got != c.want {
			t.Errorf("isMeaninglessAddr(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

// TestLookupProcessByPort 验证端口→进程确定性识别：
// 空闲端口返回空；活跃监听端口能查到 PID 与进程名。
// ss/lsof 都不可用时跳过（精简容器/非 Linux 环境）。
func TestLookupProcessByPort(t *testing.T) {
	if !havePortTool() {
		t.Skip("ss/lsof 均不可用，跳过端口进程查询测试")
	}
	// 1. 空闲端口 → 返回空（先监听拿一个空闲端口号，关闭后该端口无进程占用）
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	freePort := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	_ = ln.Close()
	if p := lookupProcessByPort(freePort); p.Name != "" || p.PID != "" || p.Cmdline != "" {
		t.Errorf("空闲端口 %s 查到进程 %+v，want 空", freePort, p)
	}
	// 2. 活跃监听端口 → 能查到 PID/进程名（本测试进程自己监听的 socket）
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln2.Close()
	activePort := strconv.Itoa(ln2.Addr().(*net.TCPAddr).Port)
	p := lookupProcessByPort(activePort)
	if p.PID == "" || p.Name == "" {
		t.Fatalf("监听端口 %s 未查到进程（ss/lsof 输出可能受限）: %+v", activePort, p)
	}
	if p.Cmdline == "" {
		t.Logf("端口 %s 查到进程 %s(pid=%s)，但 cmdline 为空（权限受限）", activePort, p.Name, p.PID)
	}
}

// havePortTool 探测 ss / lsof 是否至少有一个可用。
func havePortTool() bool {
	for _, tool := range []string{"ss", "lsof"} {
		if _, err := exec.LookPath(tool); err == nil {
			return true
		}
	}
	return false
}

// TestClassifyFromCmdline 验证 cmdline 确定性识别：
// 明确程序标识直接判定；裸解释器（python3/java）判定为模糊（标记未知）。
func TestClassifyFromCmdline(t *testing.T) {
	cases := []struct {
		name    string
		proc    procInfo
		program string
		conf    float64
		ok      bool
	}{
		{"hermes cmdline", procInfo{Name: "python3", PID: "100", Cmdline: "python3 hermes-agent --daemon"},
			"Hermes", 0.9, true},
		{"claude cmdline", procInfo{Name: "claude", PID: "101", Cmdline: "/usr/local/bin/claude"},
			"Claude Code", 0.9, true},
		{"node pi", procInfo{Name: "node", PID: "102", Cmdline: "node /usr/bin/pi"},
			"pi agent", 0.9, true},
		{"curl", procInfo{Name: "curl", PID: "103", Cmdline: "curl -s https://example.com"},
			"curl", 0.9, true},
		{"python script", procInfo{Name: "python3", PID: "104", Cmdline: "python3 /opt/agent.py --server"},
			"agent", 0.85, true},
		{"bare python3", procInfo{Name: "python3", PID: "105", Cmdline: "python3"},
			"", 0, false},
		{"bare java", procInfo{Name: "java", PID: "106", Cmdline: "java -Xmx1g"},
			"", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			program, conf, evid, ok := classifyFromCmdline("53211", c.proc)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v (program=%q evid=%q)", ok, c.ok, program, evid)
			}
			if !ok {
				return
			}
			if program != c.program || conf != c.conf {
				t.Errorf("got (%q, %v), want (%q, %v)", program, conf, c.program, c.conf)
			}
		})
	}
}

// testClientAnalysisConfig 构造开启客户端分析的配置。
func testClientAnalysisConfig() *config.Config {
	cfg := testConfig()
	cfg.Proxy.ClientAnalysis = true
	cfg.Proxy.ClientAnalysisInterval = 600
	return cfg
}

// TestClassifyFromUA 验证 User-Agent 判定：已知前缀直接命中（含大小写变体与逗号
// 分隔的 UA 集合），未知 UA 返回 ok=false 走后续推断。
func TestClassifyFromUA(t *testing.T) {
	cases := []struct {
		name    string
		ua      string
		program string
		ok      bool
	}{
		{"hermes", "Hermes/0.5.2", "Hermes", true},
		{"claude-cli", "claude-cli/1.0.66 (Claude Code)", "Claude Code", true},
		{"pi agent", "pi/0.83.0", "pi agent", true},
		{"curl", "curl/8.5.0", "curl", true},
		{"curl upper", "Curl/8.1.2", "curl", true},
		{"python-requests", "python-requests/2.32.3", "python (requests)", true},
		{"node-fetch", "node-fetch/3.3.2", "node (fetch)", true},
		{"opencode", "opencode/0.2.0", "OpenCode", true},
		{"codex contain", "Mozilla/5.0 (Codex CLI)", "Codex", true}, // 无斜杠条目子串匹配
		{"cursor contain", "Cursor/0.44.7", "Cursor", true},
		{"cherry contain", "Cherry Studio/1.2.0", "Cherry Studio", true},
		{"pi-agent contain", "pi-agent/0.1.0", "pi agent", true},
		{"go-http-client", "Go-http-client/1.1", "Go HTTP", true},
		{"openai prefix", "OpenAI/Go 1.2.0", "OpenAI SDK", true},
		{"ua set", "Hermes/0.5.2, python-requests/2.32.3", "Hermes", true},
		{"ua set second hits", "python-requests/2.32.3, claude-cli/1.0.66", "python (requests)", true},
		{"empty", "", "", false},
		{"unknown", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36", "", false},
		{"unknown with pi-like", "apifox/2.5.0", "", false}, // api/ 不匹配 pi/，不能误伤
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			program, _, evid, ok := classifyFromUA(c.ua)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v (program=%q evid=%q)", ok, c.ok, program, evid)
			}
			if !ok {
				return
			}
			if program != c.program {
				t.Errorf("program = %q, want %q", program, c.program)
			}
			if !strings.Contains(evid, "User-Agent:") {
				t.Errorf("evidence = %q, want contains User-Agent:", evid)
			}
		})
	}
}

// TestAnalyzer_RunOnce 端到端验证分析流程：
// 聚合去重 client_addr → 全量分析（本机地址不再过滤）→ 识别函数推断 → upsert 落库。
func TestAnalyzer_RunOnce(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()

	now := time.Now().UTC()
	// 两条外部地址（192.168.1.20 出现 2 次，192.168.1.21 出现 1 次）+ 一条本机地址（保留，不过滤）
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
	// 注入假识别函数（替换真实 UA/端口查进程流程）：按 addr 返回不同结果
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
	if resp.Analyzed != 3 {
		t.Errorf("analyzed = %d, want 3（本机 127.0.0.1 不再过滤）", resp.Analyzed)
	}
	if resp.NewProfiles != 3 {
		t.Errorf("new_profiles = %d, want 3", resp.NewProfiles)
	}

	// 落库检查
	profiles, err := s.ListClientProfiles()
	if err != nil {
		t.Fatalf("ListClientProfiles: %v", err)
	}
	if len(profiles) != 3 {
		t.Fatalf("profiles count = %d, want 3", len(profiles))
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
	// 本机地址保留并正常分析（不再过滤）
	if p := byAddr["127.0.0.1:5555"]; p.Program != "OpenCode" || p.Confidence != 0.7 {
		t.Errorf("127.0.0.1:5555 profile = %+v, want OpenCode/0.7（本机地址不再过滤）", p)
	}

	// 再次运行：重复分析应 upsert 更新而非新增（行数不变）
	if _, err := a.RunOnce(true); err != nil {
		t.Fatalf("RunOnce second: %v", err)
	}
	profiles2, _ := s.ListClientProfiles()
	if len(profiles2) != 3 {
		t.Errorf("profiles after second run = %d, want 3（upsert 去重）", len(profiles2))
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

// TestAnalysisProfiles_CacheFields 验证 GET /admin/analysis/profiles 每条档案附带
// 缓存命中率统计（cache_max/cache_min/cache_avg，百分比 0-100）；无统计的为 null。
func TestAnalysisProfiles_CacheFields(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()
	// 两个 profile；addr1 有缓存统计，addr2 无任何日志
	for _, p := range []store.ClientProfile{
		{ClientAddr: "192.168.1.20:53211", Program: "Hermes", Confidence: 0.8, Evidence: "UA=Hermes/0.5.2"},
		{ClientAddr: "192.168.1.21:6000", Program: "未知", Confidence: 0.1, Evidence: "UA 未识别"},
	} {
		if err := s.UpsertClientProfile(p); err != nil {
			t.Fatalf("UpsertClientProfile: %v", err)
		}
	}
	// addr1 两条日志：命中率 50% 与 100% → max=100 min=50 avg=75
	now := time.Now()
	for _, rec := range []store.Record{
		{Timestamp: now, Upstream: "up", Model: "m", Endpoint: "chat", Status: 200, ClientAddr: "192.168.1.20:53211", PromptTokens: 100, PromptCacheHitTokens: 50},
		{Timestamp: now, Upstream: "up", Model: "m", Endpoint: "chat", Status: 200, ClientAddr: "192.168.1.20:53211", PromptTokens: 200, PromptCacheHitTokens: 200},
	} {
		if err := s.Insert(rec); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	h, hc := newTestHandler(t, testConfig())
	defer hc.Close()
	h.SetStore(s)

	rr := httptest.NewRecorder()
	h.AnalysisProfiles(rr, httptest.NewRequest(http.MethodGet, "/admin/analysis/profiles", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}
	var out []profileWithCacheStats
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("profiles len = %d, want 2", len(out))
	}
	var withStat, withoutStat *profileWithCacheStats
	for i := range out {
		if out[i].ClientAddr == "192.168.1.20:53211" {
			withStat = &out[i]
		} else {
			withoutStat = &out[i]
		}
	}
	if withStat == nil || withStat.CacheMax == nil || withStat.CacheMin == nil || withStat.CacheAvg == nil {
		t.Fatalf("addr1 cache 字段应为非空，got %+v", withStat)
	}
	if *withStat.CacheMax != 100 || *withStat.CacheMin != 50 || *withStat.CacheAvg != 75 {
		t.Errorf("addr1 cache = max=%v min=%v avg=%v, want 100/50/75", *withStat.CacheMax, *withStat.CacheMin, *withStat.CacheAvg)
	}
	if withoutStat == nil || withoutStat.CacheMax != nil || withoutStat.CacheMin != nil || withoutStat.CacheAvg != nil {
		t.Errorf("addr2 cache 字段应为 null，got %+v", withoutStat)
	}
	// 原有字段（嵌入 ClientProfile）不被破坏
	if withStat.Program != "Hermes" || withStat.Confidence != 0.8 {
		t.Errorf("addr1 program/confidence = %+v, want Hermes/0.8", withStat)
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
