package config

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/icefairy/xuanji/internal/store"
)

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

const sampleConfig = `
server:
  port: 8787

upstreams:
  - name: vllm-local
    type: openai
    base_url: http://127.0.0.1:3001/v1
    api_key: ${VLLM_KEY}
    priority: 1
    weight: 100
    models:
      - qwen3.6:35b
    health_check:
      path: /models
      interval: 30s
      timeout: 5s

routing:
  default_strategy: primary_backup
  rules:
    - model: "qwen*"
      upstreams: [vllm-local]
`

func TestLoad_ExpandsDotEnv(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"config.yaml": sampleConfig,
		".env":        "VLLM_KEY=sk-secret-from-env\n",
	})

	cfg, err := Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Upstreams[0].APIKey; got != "sk-secret-from-env" {
		t.Errorf("APIKey = %q, want expanded value from .env", got)
	}
	if got := cfg.Upstreams[0].HealthCheck.Interval.String(); got != "30s" {
		t.Errorf("HealthCheck.Interval = %q, want 30s", got)
	}
}

func TestLoad_DoesNotOverrideExistingEnv(t *testing.T) {
	t.Setenv("VLLM_KEY", "sk-existing")
	dir := writeFiles(t, map[string]string{
		"config.yaml": sampleConfig,
		".env":        "VLLM_KEY=sk-from-dotenv\n",
	})

	cfg, err := Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Upstreams[0].APIKey; got != "sk-existing" {
		t.Errorf("APIKey = %q, want existing process env to win", got)
	}
}

func TestLoad_AppliesDefaults(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"config.yaml": `
upstreams:
  - name: u1
    base_url: http://example.com/v1
    api_key: ${K}
    models: [m1]
`,
		".env": "K=secret\n",
	})

	cfg, err := Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Port != DefaultPort {
		t.Errorf("Port = %d, want default %d", cfg.Server.Port, DefaultPort)
	}
	if cfg.Routing.DefaultStrategy != DefaultStrategy {
		t.Errorf("DefaultStrategy = %q, want default %q", cfg.Routing.DefaultStrategy, DefaultStrategy)
	}
}

func TestLoad_ValidationErrors(t *testing.T) {
	cases := []struct {
		name    string
		config  string
		wantErr string
	}{
		{
			name:    "no upstreams",
			config:  "server:\n  port: 8787\n",
			wantErr: "at least one upstream",
		},
		{
			name: "missing api_key",
			config: `
upstreams:
  - name: u1
    base_url: http://example.com/v1
    models: [m1]
`,
			wantErr: "api_key is required",
		},
		{
			name: "missing base_url",
			config: `
upstreams:
  - name: u1
    api_key: k
    models: [m1]
`,
			wantErr: "base_url is required",
		},
		{
			name: "empty models",
			config: `
upstreams:
  - name: u1
    base_url: http://example.com/v1
    api_key: k
`,
			wantErr: "models must not be empty",
		},
		{
			name: "invalid default strategy",
			config: `
upstreams:
  - name: u1
    base_url: http://example.com/v1
    api_key: k
    models: [m1]
routing:
  default_strategy: random
`,
			wantErr: "default_strategy",
		},
		{
			name: "rule without upstreams",
			config: `
upstreams:
  - name: u1
    base_url: http://example.com/v1
    api_key: k
    models: [m1]
routing:
  rules:
    - model: "m1"
`,
			wantErr: "upstreams must not be empty",
		},
		{
			name: "invalid kind",
			config: `
upstreams:
  - name: u1
    base_url: http://example.com/v1
    api_key: k
    models: [m1]
    kind: video-gen
`,
			wantErr: `kind "video-gen" is invalid`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeFiles(t, map[string]string{"config.yaml": tc.config})
			_, err := Load(filepath.Join(dir, "config.yaml"))
			if err == nil {
				t.Fatalf("Load: expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want containing %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// openTestStore 在临时目录打开一个 store，SeedDefaults 并返回。
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.SeedDefaults(); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}
	return s
}

func TestLoadFromDB_Defaults(t *testing.T) {
	s := openTestStore(t)

	cfg, err := LoadFromDB(s)
	if err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}
	if cfg.Server.Port != DefaultPort {
		t.Errorf("Port = %d, want %d", cfg.Server.Port, DefaultPort)
	}
	if cfg.Retry.MaxRetries != 3 {
		t.Errorf("MaxRetries = %d, want 3", cfg.Retry.MaxRetries)
	}
	if len(cfg.Retry.RetryStatuses) != 5 || cfg.Retry.RetryStatuses[0] != 429 {
		t.Errorf("RetryStatuses = %v, want default [429 500 502 503 504]", cfg.Retry.RetryStatuses)
	}
	if len(cfg.Retry.RetryKeywords) != 4 {
		t.Errorf("RetryKeywords = %v, want 4 defaults", cfg.Retry.RetryKeywords)
	}
	if cfg.Retry.FastFailMinutes != 5 {
		t.Errorf("FastFailMinutes = %d, want 5", cfg.Retry.FastFailMinutes)
	}
	if cfg.Retry.FastFailProbeMinutes != 5 {
		t.Errorf("FastFailProbeMinutes = %d, want 5", cfg.Retry.FastFailProbeMinutes)
	}
	if cfg.Routing.DefaultStrategy != DefaultStrategy {
		t.Errorf("DefaultStrategy = %q, want %q", cfg.Routing.DefaultStrategy, DefaultStrategy)
	}
	// developer 角色兼容默认开启：config 表未显式存该 key 时应为 true
	if !cfg.Proxy.NormalizeDeveloperRole {
		t.Errorf("NormalizeDeveloperRole = false, want default true")
	}
	// reasoning_content 回传缓存默认开启：config 表未显式存该 key 时应为 true
	if !cfg.Proxy.CacheReasoningContent {
		t.Errorf("CacheReasoningContent = false, want default true")
	}
}

func TestLoadFromDB_CacheReasoningContent(t *testing.T) {
	s := openTestStore(t)

	// 显式 false → 关闭（缓存不写、注入不执行，body 原样透传）
	if err := s.SetConfig("proxy.cache_reasoning_content", "false"); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	cfg, err := LoadFromDB(s)
	if err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}
	if cfg.Proxy.CacheReasoningContent {
		t.Errorf("CacheReasoningContent = true, want false (explicit false)")
	}

	// 显式 true → 开启
	if err := s.SetConfig("proxy.cache_reasoning_content", "true"); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	cfg, err = LoadFromDB(s)
	if err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}
	if !cfg.Proxy.CacheReasoningContent {
		t.Errorf("CacheReasoningContent = false, want true (explicit true)")
	}
}

func TestLoadFromDB_NormalizeDeveloperRole(t *testing.T) {
	s := openTestStore(t)

	// 显式 false → 关闭
	if err := s.SetConfig("proxy.normalize_developer_role", "false"); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	cfg, err := LoadFromDB(s)
	if err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}
	if cfg.Proxy.NormalizeDeveloperRole {
		t.Errorf("NormalizeDeveloperRole = true, want false (explicit false)")
	}

	// 显式 true → 开启
	if err := s.SetConfig("proxy.normalize_developer_role", "true"); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	cfg, err = LoadFromDB(s)
	if err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}
	if !cfg.Proxy.NormalizeDeveloperRole {
		t.Errorf("NormalizeDeveloperRole = false, want true (explicit true)")
	}
}

func TestLoadFromDB_CustomValues(t *testing.T) {
	s := openTestStore(t)

	if err := s.SetConfig("server.port", "9999"); err != nil {
		t.Fatalf("SetConfig port: %v", err)
	}
	if err := s.SetConfig("retry.max_retries", "5"); err != nil {
		t.Fatalf("SetConfig max_retries: %v", err)
	}
	if err := s.SetConfig("retry.retry_statuses", "429,503"); err != nil {
		t.Fatalf("SetConfig statuses: %v", err)
	}
	if err := s.SetConfig("retry.retry_keywords", "quota,余额不足"); err != nil {
		t.Fatalf("SetConfig keywords: %v", err)
	}
	if err := s.SetConfig("retry.fast_fail_minutes", "10"); err != nil {
		t.Fatalf("SetConfig fast_fail: %v", err)
	}

	cfg, err := LoadFromDB(s)
	if err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}
	if cfg.Server.Port != 9999 {
		t.Errorf("Port = %d, want 9999", cfg.Server.Port)
	}
	if cfg.Retry.MaxRetries != 5 {
		t.Errorf("MaxRetries = %d, want 5", cfg.Retry.MaxRetries)
	}
	if len(cfg.Retry.RetryStatuses) != 2 || cfg.Retry.RetryStatuses[0] != 429 || cfg.Retry.RetryStatuses[1] != 503 {
		t.Errorf("RetryStatuses = %v, want [429 503]", cfg.Retry.RetryStatuses)
	}
	if len(cfg.Retry.RetryKeywords) != 2 || cfg.Retry.RetryKeywords[0] != "quota" {
		t.Errorf("RetryKeywords = %v, want [quota 余额不足]", cfg.Retry.RetryKeywords)
	}
	if cfg.Retry.FastFailMinutes != 10 {
		t.Errorf("FastFailMinutes = %d, want 10", cfg.Retry.FastFailMinutes)
	}
}

func TestLoadFromDB_UpstreamsAndRules(t *testing.T) {
	s := openTestStore(t)

	models, _ := json.Marshal([]string{"deepseek-v4-flash", "bge-m3"})
	if err := s.CreateUpstream(&store.UpstreamRow{
		Name:     "u1",
		Type:     "openai",
		BaseURL:  "http://a.local",
		APIKey:   "sk-a",
		Tier:     "payg",
		Priority: 1,
		Weight:   100,
		Models:   string(models),
	}); err != nil {
		t.Fatalf("CreateUpstream: %v", err)
	}
	ups, _ := json.Marshal([]string{"u1"})
	if err := s.CreateRoutingRule(&store.RoutingRuleRow{
		Model:     "deepseek-v4-flash",
		Strategy:  "primary_backup",
		Upstreams: string(ups),
	}); err != nil {
		t.Fatalf("CreateRoutingRule: %v", err)
	}

	cfg, err := LoadFromDB(s)
	if err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}
	if len(cfg.Upstreams) != 1 {
		t.Fatalf("len(Upstreams) = %d, want 1", len(cfg.Upstreams))
	}
	u := cfg.Upstreams[0]
	if u.Name != "u1" || u.BaseURL != "http://a.local" || u.Priority != 1 {
		t.Errorf("upstream = %+v, want u1/http://a.local/prio1", u)
	}
	if len(u.Models) != 2 || u.Models[0] != "deepseek-v4-flash" {
		t.Errorf("upstream.Models = %v, want [deepseek-v4-flash bge-m3]", u.Models)
	}
	if len(cfg.Routing.Rules) != 1 {
		t.Fatalf("len(Rules) = %d, want 1", len(cfg.Routing.Rules))
	}
	r := cfg.Routing.Rules[0]
	if r.Model != "deepseek-v4-flash" || r.Strategy != "primary_backup" {
		t.Errorf("rule = %+v, want deepseek-v4-flash/primary_backup", r)
	}
	if len(r.Upstreams) != 1 || r.Upstreams[0] != "u1" {
		t.Errorf("rule.Upstreams = %v, want [u1]", r.Upstreams)
	}
}

// TestLoadFromDB_ClientAnalysis 验证客户端分析配置的加载与默认值：
// 默认关闭 + 间隔 600；显式配置后生效；非法间隔回退默认。
func TestLoadFromDB_ClientAnalysis(t *testing.T) {
	s := openTestStore(t)

	// 默认：未配置任何 key → 关闭 + 间隔 600
	cfg, err := LoadFromDB(s)
	if err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}
	if cfg.Proxy.ClientAnalysis {
		t.Errorf("ClientAnalysis = true, want default false")
	}
	if cfg.Proxy.ClientAnalysisInterval != 600 {
		t.Errorf("ClientAnalysisInterval = %d, want default 600", cfg.Proxy.ClientAnalysisInterval)
	}

	// 显式开启 + 1 分钟
	if err := s.SetConfig("proxy.client_analysis", "true"); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if err := s.SetConfig("proxy.client_analysis_interval", "60"); err != nil {
		t.Fatalf("SetConfig interval: %v", err)
	}
	cfg, err = LoadFromDB(s)
	if err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}
	if !cfg.Proxy.ClientAnalysis {
		t.Errorf("ClientAnalysis = false, want true (explicit true)")
	}
	if cfg.Proxy.ClientAnalysisInterval != 60 {
		t.Errorf("ClientAnalysisInterval = %d, want 60", cfg.Proxy.ClientAnalysisInterval)
	}

	// 非法间隔（小于 60 秒）→ 回退默认 600
	if err := s.SetConfig("proxy.client_analysis_interval", "10"); err != nil {
		t.Fatalf("SetConfig interval: %v", err)
	}
	cfg, err = LoadFromDB(s)
	if err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}
	if cfg.Proxy.ClientAnalysisInterval != 600 {
		t.Errorf("ClientAnalysisInterval = %d, want 600 (invalid falls back)", cfg.Proxy.ClientAnalysisInterval)
	}
}

// TestLoad_KindDefaults 验证上游能力字段（kind）：留空默认 chat；显式配置各合法值原样保留。
func TestLoad_KindDefaults(t *testing.T) {
	dir := writeFiles(t, map[string]string{"config.yaml": `
upstreams:
  - name: chat-up
    base_url: http://example.com/v1
    api_key: k
    models: [m1]
  - name: tts-up
    base_url: http://example.com/v1
    api_key: k
    models: [tts-1]
    kind: tts
  - name: emb-up
    base_url: http://example.com/v1
    api_key: k
    models: [bge-m3]
    kind: emb
`})
	cfg, err := Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := map[string]string{"chat-up": "chat", "tts-up": "tts", "emb-up": "emb"}
	for _, up := range cfg.Upstreams {
		if got := want[up.Name]; up.Kind != got {
			t.Errorf("upstream %q kind = %q, want %q", up.Name, up.Kind, got)
		}
	}
}

// TestLoadFromDB_InvalidModelMappingLogsError 回归测试（2026-09-14 实测事故）：
// model_mapping 是非法 JSON 时，json.Unmarshal 不做部分填充（整个 map 为空，实测
// len(m)==0），该上游**全部**模型映射静默丢失，后续把客户端短名原样透传给上游
// （上游回 400 "Model does not exist"）。修复后加载侧必须打 ERROR 告警，
// 否则故障现场只能看到下游 400，看不到根因（当天排查耗时很久）。
func TestLoadFromDB_InvalidModelMappingLogsError(t *testing.T) {
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	s := openTestStore(t)
	// 真实事故形态：`"chat":"X""asr":"Y"` 缺逗号
	broken := `{"bge-m3":"BAAI/bge-m3","chat":"THUDM/GLM-4-9B-0414""asr":"FunAudioLLM/SenseVoiceSmall"}`
	if err := s.CreateUpstream(&store.UpstreamRow{
		Name: "lxr", Type: "openai", BaseURL: "http://a.local", APIKey: "sk-a",
		Tier: "free", Priority: 1, Weight: 100, Models: `["bge-m3","asr"]`,
		ModelMapping: broken,
	}); err != nil {
		t.Fatalf("CreateUpstream: %v", err)
	}

	cfg, err := LoadFromDB(s)
	if err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}

	logs := buf.String()
	if !strings.Contains(logs, "model_mapping") || !strings.Contains(logs, "ERROR") {
		t.Errorf("非法 model_mapping 必须打 ERROR 告警，实际日志：%q", logs)
	}
	// 映射确实丢失（这正是要告警的危害，不要试图"修复"填充：语法错误无可靠部分解析）
	if len(cfg.Upstreams) != 1 {
		t.Fatalf("len(Upstreams) = %d, want 1", len(cfg.Upstreams))
	}
	if got := cfg.Upstreams[0].ModelMapping; len(got) != 0 {
		t.Errorf("非法 JSON 的 ModelMapping = %v, want 空（Go 解析失败不部分填充）", got)
	}
}

// TestLoadFromDB_ValidModelMappingNoError 反向保护：合法配置不得产生 ERROR 噪音。
func TestLoadFromDB_ValidModelMappingNoError(t *testing.T) {
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	s := openTestStore(t)
	if err := s.CreateUpstream(&store.UpstreamRow{
		Name: "ok", Type: "openai", BaseURL: "http://a.local", APIKey: "sk-a",
		Tier: "free", Priority: 1, Weight: 100, Models: `["bge-m3"]`,
		ModelMapping: `{"bge-m3":"BAAI/bge-m3"}`,
	}); err != nil {
		t.Fatalf("CreateUpstream: %v", err)
	}
	cfg, err := LoadFromDB(s)
	if err != nil {
		t.Fatalf("LoadFromDB: %v", err)
	}
	if strings.Contains(buf.String(), "model_mapping") {
		t.Errorf("合法 model_mapping 不应告警：%q", buf.String())
	}
	if got := cfg.Upstreams[0].ModelMapping["bge-m3"]; got != "BAAI/bge-m3" {
		t.Errorf("映射丢失：got %q, want BAAI/bge-m3", got)
	}
}
