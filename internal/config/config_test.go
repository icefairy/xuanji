package config

import (
	"encoding/json"
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
    base_url: http://192.168.1.10:3001/v1
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
	if cfg.Retry.FastFailMinutes != 60 {
		t.Errorf("FastFailMinutes = %d, want 60", cfg.Retry.FastFailMinutes)
	}
	if cfg.Retry.FastFailProbeMinutes != 35 {
		t.Errorf("FastFailProbeMinutes = %d, want 35", cfg.Retry.FastFailProbeMinutes)
	}
	if cfg.Routing.DefaultStrategy != DefaultStrategy {
		t.Errorf("DefaultStrategy = %q, want %q", cfg.Routing.DefaultStrategy, DefaultStrategy)
	}
}

func TestLoadFromDB_CustomValues(t *testing.T) {
	s := openTestStore(t)

	if err := s.SetConfig("server.port", "9999"); err != nil {
		t.Fatalf("SetConfig port: %v", err)
	}
	if err := s.SetConfig("server.api_keys", "sk-admin-1,sk-admin-2"); err != nil {
		t.Fatalf("SetConfig api_keys: %v", err)
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
	if cfg.Server.APIKeys != "sk-admin-1,sk-admin-2" {
		t.Errorf("APIKeys = %q, want sk-admin-1,sk-admin-2", cfg.Server.APIKeys)
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
