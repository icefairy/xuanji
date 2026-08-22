// Package config 负责加载与校验璇玑网关的 YAML 配置。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/icefairy/xuanji/internal/store"
	"gopkg.in/yaml.v3"
)

// Duration 是带单位的时间长度（如 "30s"、"5h"），用于 YAML 配置反序列化。
type Duration time.Duration

// UnmarshalYAML 将 "30s" 之类的时间字符串解析为 Duration，空值视为 0。
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("invalid duration: %w", err)
	}
	if strings.TrimSpace(raw) == "" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", raw, err)
	}
	*d = Duration(v)
	return nil
}

// String 返回 duration 的标准字符串形式。
func (d Duration) String() string { return time.Duration(d).String() }

// Retry 描述重试策略。
type Retry struct {
	MaxRetries           int      `yaml:"max_retries"`             // 最多重试次数（跨上游切换），默认 3
	RetryStatuses        []int    `yaml:"retry_statuses"`          // 触发重试的 HTTP 状态码，默认 [429, 500, 502, 503, 504]
	RetryKeywords        []string `yaml:"retry_keywords"`          // 响应体中含这些关键词则触发重试
	FastFailMinutes      int      `yaml:"fast_fail_minutes"`       // 上游失败后跳过尝试的分钟数，默认 5
	FastFailProbeMinutes int      `yaml:"fast_fail_probe_minutes"` // 后台探测被禁用上游的间隔分钟数，默认 35
	UpstreamTimeout      int      `yaml:"upstream_timeout"`        // 上游请求超时秒数（连接+非流式整体），默认 60
}

// Config 是网关的根配置结构，与 config.yaml 严格对应。
type Config struct {
	Server    Server     `yaml:"server"`
	Upstreams []Upstream `yaml:"upstreams"`
	Routing   Routing    `yaml:"routing"`
	Retry     Retry      `yaml:"retry"`
	Proxy     Proxy      `yaml:"proxy"`
	Storage   Storage    `yaml:"storage"`
}

// Proxy 描述转发层的可选行为开关。
type Proxy struct {
	// AutoBestEffort 开启后，客户端未传 reasoning_effort 时，
	// 自动按 EffortConfigs 的推荐值补上（默认 false）。
	AutoBestEffort bool `yaml:"auto_best_effort"`
	// ForceBestEffort 开启后，强制用 EffortConfigs 的强制值覆盖客户端传的 reasoning_effort（默认 false）。
	ForceBestEffort bool `yaml:"force_best_effort"`
	// VideoPassThrough 开启后，允许 content 中的 video_url 透传给上游（多模态视频，默认 false）。
	// 关闭时请求含 video_url 直接返回 400——视频流量大，需显式开启。
	VideoPassThrough bool `yaml:"video_pass_through"`
	// CacheReasoningContent 开启后，缓存上游响应中 assistant 消息的 reasoning_content
	// （按 tool_call_id），并在后续请求中为丢失该字段的 assistant 消息自动补回
	// （DeepSeek thinking 模式 tool-calling 要求 reasoning_content 原样回传，否则上游
	// 400；默认 true）。关闭时缓存不写、注入不执行，body 原样透传。
	CacheReasoningContent bool `yaml:"cache_reasoning_content"`
	// NormalizeDeveloperRole 开启后，转发前把 messages 中的 role=developer 归一化为 role=system（默认 true）。
	// 部分上游（商汤/基元律动等）不认 OpenAI 新角色 developer，只认 system/user/assistant/tool。
	NormalizeDeveloperRole bool `yaml:"normalize_developer_role"`
	// CooldownUpstreams 是需要 per-key 冷却的上游名称前缀列表。
	// 同一上游每次成功响应后，暂停 CooldownSeconds 秒不再被分配新请求，
	// 防止同一 API key 被并发打爆触发 429。
	// 例：["商汤"] 会匹配 "商汤陈永鹏""商汤-陈俊" 等所有商汤上游。
	CooldownUpstreams []string `yaml:"cooldown_upstreams"`
	// CooldownSeconds 是每个上游成功请求后的冷却秒数，默认 1。
	CooldownSeconds int `yaml:"cooldown_seconds"`
	// EffortConfigs 是最佳思考等级配置（模型 pattern → 推荐/强制等级）。
	EffortConfigs []EffortConfig `yaml:"effort_configs"`
	// ClientAnalysis 开启后，定期分析 client_addr 对应的调用程序（默认 false）。
	// 聚合最近 N 分钟去重的 client_addr，按 User-Agent → 端口查进程识别程序名，
	// 结果存 client_profiles 表。
	ClientAnalysis bool `yaml:"client_analysis"`
	// ClientAnalysisInterval 分析间隔秒数（默认 600，即 10 分钟）。
	ClientAnalysisInterval int `yaml:"client_analysis_interval"`
	// UserAgent 是转发到上游时设置的 User-Agent。留空时用 DefaultUpstreamUserAgent
	// （pi agent 的 UA）。改动机：部分上游用 UA 判断调用方是否官方客户端，
	// 设置自定义 UA 可避免 Go 默认的 Go-http-client/1.1 被误判为脚本/网关程序。
	UserAgent string `yaml:"user_agent"`
}

// EffortConfig 描述单个模型的最佳思考等级配置。
type EffortConfig struct {
	Model       string `yaml:"model"`       // 模型匹配 pattern（支持 * 通配）
	Recommended string `yaml:"recommended"` // 推荐等级（客户端未传时自动补）
	Forced      string `yaml:"forced"`      // 强制等级（覆盖客户端传的）
}

// Storage 是持久化配置。
type Storage struct {
	// SQLitePath 是 SQLite 数据库文件路径；为空则不持久化（仅内存转发）。
	SQLitePath string `yaml:"sqlite_path"`
}

// Server 是 HTTP 服务配置。
type Server struct {
	Port int `yaml:"port"`
	// AdminAPIKey 是管理 API（/api/admin/*）的独立鉴权 key。
	// 供 AI 助手等自动化工具免登录调用动态管理接口；存 config 表 admin.api_key。
	AdminAPIKey string `yaml:"admin_api_key"`
}

// Upstream 描述一个上游端点。Type 决定协议：
//
//	openai / openai-compatible —— OpenAI 兼容（/v1/chat/completions 等）
//	anthropic —— Anthropic 原生（/v1/messages）
//	ollama —— Ollama 原生（/api/chat, /api/generate, /api/embed）
type Upstream struct {
	Name            string            `yaml:"name"`
	Type            string            `yaml:"type"`
	BaseURL         string            `yaml:"base_url"`
	APIKey          string            `yaml:"api_key"`
	Tier            string            `yaml:"tier"` // free | subscription | payg（默认 payg）
	Priority        int               `yaml:"priority"`
	Weight          int               `yaml:"weight"`
	Models          []string          `yaml:"models"`
	ModelMapping    map[string]string `yaml:"model_mapping"`    // 客户端模型名 → 上游真实模型名
	RequestOverride string            `yaml:"request_override"` // 请求体复写（JSON 字符串）：转发前强制覆盖请求体部分字段，空=不启用
	Enabled         bool              `yaml:"enabled"`          // 禁用（false）时不参与转发路由
	MaxTokensCap    int               `yaml:"max_tokens_cap"`   // 上游 max_tokens 上限；0=不限制（客户端传超范围值时 clamp 到该值，防 400）
	Quota           *Quota            `yaml:"quota"`
	HealthCheck     *HealthCheck      `yaml:"health_check"`
}

// IsOllama 判断上游是否为 Ollama 原生协议。
func (u *Upstream) IsOllama() bool {
	return strings.EqualFold(u.Type, "ollama")
}

// IsDots 判断上游是否为 Dots API 开放平台（dots.ai）。
// Dots 的特殊性：认证用 api-key 请求头（非 Authorization Bearer），
// 图片要求标准 OpenAI 嵌套格式（image_url:{url}，不接受拍平字符串）。
func (u *Upstream) IsDots() bool {
	return strings.EqualFold(u.Type, "dots")
}

// IsAnthropic 判断上游是否为 Anthropic 原生协议。
func (u *Upstream) IsAnthropic() bool {
	return strings.EqualFold(u.Type, "anthropic")
}

// IsGemini 判断上游是否为 Google Gemini 原生协议。
func (u *Upstream) IsGemini() bool {
	return strings.EqualFold(u.Type, "gemini")
}

// TierWeight 返回上游的成本层级权重，用于路由排序：subscription(0) < free(1) < payg(2)。
// 包月（订阅）优先——用户付费的官方服务质量最高，先用完包月额度；
// 免费次之；按量付费最不优先（按量花钱，能省则省）。
// 未配置 tier 视为 payg（按量计费，最不优先，安全默认）。
func (u *Upstream) TierWeight() int {
	switch u.Tier {
	case "subscription":
		return 0
	case "free":
		return 1
	default:
		return 2
	}
}

// Quota 描述上游的配额余量配置。
type Quota struct {
	Rolling Duration `yaml:"rolling"`
	Weekly  Duration `yaml:"weekly"`
	Monthly Duration `yaml:"monthly"`
}

// HealthCheck 描述上游的健康检查配置。
type HealthCheck struct {
	Path     string   `yaml:"path"`
	Interval Duration `yaml:"interval"`
	Timeout  Duration `yaml:"timeout"`
}

// Routing 描述模型路由与分流策略配置。
type Routing struct {
	DefaultStrategy string `yaml:"default_strategy"`
	Rules           []Rule `yaml:"rules"`
}

// Rule 是模型到上游组的单条路由规则。
type Rule struct {
	Model     string   `yaml:"model"`
	Upstreams []string `yaml:"upstreams"`
	Strategy  string   `yaml:"strategy"`
	// Vision 是否支持多模态（默认 false=不支持）。请求带图且命中本规则时，
	// 若 VisionFallback 非空则把 model 改写为兜底模型重新路由。
	Vision bool `yaml:"vision"`
	// VisionFallback 多模态兜底转发的聚合模型名（如 "flash"），
	// 由对应上游的 model_mapping 映射到上游真实模型名；空=不兜底。
	VisionFallback string `yaml:"vision_fallback"`
}

// DefaultPort 是 server.port 未配置时的默认监听端口。
const DefaultPort = 8787

// DefaultStrategy 是 routing.default_strategy 未配置时的默认分流策略。
const DefaultStrategy = "primary_backup"

// validStrategies 是可选的分流策略枚举。
var validStrategies = map[string]bool{
	"quota":          true,
	"latency":        true,
	"primary_backup": true,
	"weighted":       true,
}

// Load 从 path 读取 YAML 配置：先加载 .env 到进程环境，再展开 ${VAR} 占位符，
// 最后反序列化并校验。.env 路径默认取配置同目录的 .env，可用环境变量
// XUANJI_ENV_FILE 覆盖；已存在的环境变量不会被 .env 覆盖。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	envFile := os.Getenv("XUANJI_ENV_FILE")
	if envFile == "" {
		envFile = filepath.Join(filepath.Dir(path), ".env")
	}
	if err := loadDotEnv(envFile); err != nil {
		return nil, fmt.Errorf("load env file: %w", err)
	}

	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	expandEnvNodes(&node)

	var cfg Config
	if err := node.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// validate 应用默认值并校验配置完整性。
func (c *Config) validate() error {
	if c.Server.Port == 0 {
		c.Server.Port = DefaultPort
	}

	if len(c.Upstreams) == 0 {
		return errors.New("config: upstreams requires at least one upstream")
	}
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		if strings.TrimSpace(u.Name) == "" {
			return fmt.Errorf("config: upstreams[%d]: name is required", i)
		}
		if strings.TrimSpace(u.BaseURL) == "" {
			return fmt.Errorf("config: upstreams[%d] %q: base_url is required", i, u.Name)
		}
		// Ollama 本地实例可无 key；其余上游必须配置 api_key
		if !u.IsOllama() && strings.TrimSpace(u.APIKey) == "" {
			return fmt.Errorf("config: upstreams[%d] %q: api_key is required", i, u.Name)
		}
		if len(u.Models) == 0 {
			return fmt.Errorf("config: upstreams[%d] %q: models must not be empty", i, u.Name)
		}
		for j, m := range u.Models {
			if strings.TrimSpace(m) == "" {
				return fmt.Errorf("config: upstreams[%d] %q: models[%d] is empty", i, u.Name, j)
			}
		}
	}

	if c.Retry.MaxRetries <= 0 {
		c.Retry.MaxRetries = 3
	}
	if len(c.Retry.RetryStatuses) == 0 {
		c.Retry.RetryStatuses = []int{404, 429, 500, 502, 503, 504}
	}
	if c.Retry.FastFailMinutes <= 0 {
		c.Retry.FastFailMinutes = 5
	}
	if c.Retry.FastFailProbeMinutes <= 0 {
		c.Retry.FastFailProbeMinutes = 5
	}

	if c.Routing.DefaultStrategy == "" {
		c.Routing.DefaultStrategy = DefaultStrategy
	}
	if !validStrategies[c.Routing.DefaultStrategy] {
		return fmt.Errorf("config: routing.default_strategy %q is invalid (want quota|latency|primary_backup|weighted)", c.Routing.DefaultStrategy)
	}
	for i := range c.Routing.Rules {
		r := &c.Routing.Rules[i]
		if strings.TrimSpace(r.Model) == "" {
			return fmt.Errorf("config: routing.rules[%d]: model is required", i)
		}
		if len(r.Upstreams) == 0 {
			return fmt.Errorf("config: routing.rules[%d] %q: upstreams must not be empty", i, r.Model)
		}
		if r.Strategy == "" {
			r.Strategy = c.Routing.DefaultStrategy
		}
		if !validStrategies[r.Strategy] {
			return fmt.Errorf("config: routing.rules[%d] %q: strategy %q is invalid", i, r.Model, r.Strategy)
		}
	}
	return nil
}

// expandEnvNodes 递归展开 YAML 树中所有标量节点的环境变量占位符。
func expandEnvNodes(n *yaml.Node) {
	if n.Kind == yaml.ScalarNode {
		n.Value = os.ExpandEnv(n.Value)
		return
	}
	for _, child := range n.Content {
		expandEnvNodes(child)
	}
}

// LoadFromDB 从数据库加载完整配置：server/retry 键值来自 config 表，
// 上游与路由规则来自 upstreams/routing_rules 表。
func LoadFromDB(s *store.Store) (*Config, error) {
	all, err := s.GetAllConfig()
	if err != nil {
		return nil, fmt.Errorf("get all config: %w", err)
	}

	cfg := &Config{
		Routing: Routing{
			DefaultStrategy: DefaultStrategy,
		},
	}

	// 解析端口
	if v, ok := all["server.port"]; ok {
		if port, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && port > 0 {
			cfg.Server.Port = port
		}
	}

	// 解析管理 API key（/api/admin/* 独立鉴权，供 AI 助手调用）
	if v, ok := all["admin.api_key"]; ok {
		cfg.Server.AdminAPIKey = strings.TrimSpace(v)
	}

	// 解析重试配置
	if v, ok := all["retry.max_retries"]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			cfg.Retry.MaxRetries = n
		}
	}
	if v, ok := all["retry.retry_statuses"]; ok {
		var statuses []int
		for _, s := range strings.Split(v, ",") {
			s = strings.TrimSpace(s)
			if code, err := strconv.Atoi(s); err == nil {
				statuses = append(statuses, code)
			}
		}
		if len(statuses) > 0 {
			cfg.Retry.RetryStatuses = statuses
		}
	}
	if v, ok := all["retry.retry_keywords"]; ok {
		var keywords []string
		for _, kw := range strings.Split(v, ",") {
			if kw = strings.TrimSpace(kw); kw != "" {
				keywords = append(keywords, kw)
			}
		}
		if len(keywords) > 0 {
			cfg.Retry.RetryKeywords = keywords
		}
	}
	if v, ok := all["retry.fast_fail_minutes"]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			cfg.Retry.FastFailMinutes = n
		}
	}
	if v, ok := all["retry.fast_fail_probe_minutes"]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			cfg.Retry.FastFailProbeMinutes = n
		}
	}
	if v, ok := all["retry.upstream_timeout"]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			cfg.Retry.UpstreamTimeout = n
		}
	}

	// 解析转发层开关
	if v, ok := all["proxy.auto_best_effort"]; ok {
		cfg.Proxy.AutoBestEffort = strings.TrimSpace(v) == "true" || strings.TrimSpace(v) == "1"
	}
	if v, ok := all["proxy.force_best_effort"]; ok {
		cfg.Proxy.ForceBestEffort = strings.TrimSpace(v) == "true" || strings.TrimSpace(v) == "1"
	}
	if v, ok := all["proxy.video_pass_through"]; ok {
		cfg.Proxy.VideoPassThrough = strings.TrimSpace(v) == "true" || strings.TrimSpace(v) == "1"
	}
	if v, ok := all["proxy.cache_reasoning_content"]; ok {
		cfg.Proxy.CacheReasoningContent = strings.TrimSpace(v) == "true" || strings.TrimSpace(v) == "1"
	}
	if v, ok := all["proxy.normalize_developer_role"]; ok {
		cfg.Proxy.NormalizeDeveloperRole = strings.TrimSpace(v) == "true" || strings.TrimSpace(v) == "1"
	}
	if v, ok := all["proxy.client_analysis"]; ok {
		cfg.Proxy.ClientAnalysis = strings.TrimSpace(v) == "true" || strings.TrimSpace(v) == "1"
	}
	if v, ok := all["proxy.client_analysis_interval"]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 60 {
			cfg.Proxy.ClientAnalysisInterval = n
		}
	}
	if v, ok := all["proxy.cooldown_seconds"]; ok {
		if s, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && s > 0 {
			cfg.Proxy.CooldownSeconds = s
		}
	}
	// CooldownUpstreams：JSON 数组，支持逗号分隔 fallback（兼容旧格式）
	if v, ok := all["proxy.cooldown_upstreams"]; ok {
		v = strings.TrimSpace(v)
		if v != "" {
			var names []string
			if jsonErr := json.Unmarshal([]byte(v), &names); jsonErr != nil {
				// 降级为逗号分隔：逗号前后都可能有空格
				for _, n := range strings.Split(v, ",") {
					n = strings.TrimSpace(n)
					if n != "" {
						names = append(names, n)
					}
				}
			}
			cfg.Proxy.CooldownUpstreams = names
		}
	}
	// 解析转发 User-Agent（上游看到的客户端标识；空 = 用内置默认 pi agent UA）
	if v, ok := all["proxy.user_agent"]; ok {
		cfg.Proxy.UserAgent = strings.TrimSpace(v)
	}

	// 默认值
	if cfg.Server.Port == 0 {
		cfg.Server.Port = DefaultPort
	}
	if cfg.Retry.MaxRetries <= 0 {
		cfg.Retry.MaxRetries = 3
	}
	if len(cfg.Retry.RetryStatuses) == 0 {
		cfg.Retry.RetryStatuses = []int{404, 429, 500, 502, 503, 504}
	}
	if len(cfg.Retry.RetryKeywords) == 0 {
		cfg.Retry.RetryKeywords = []string{"套餐用完了", "余额不足", "quota", "rate limit"}
	}
	if cfg.Retry.FastFailMinutes <= 0 {
		cfg.Retry.FastFailMinutes = 60
	}
	if cfg.Retry.FastFailProbeMinutes <= 0 {
		cfg.Retry.FastFailProbeMinutes = 35
	}
	if cfg.Retry.UpstreamTimeout <= 0 {
		cfg.Retry.UpstreamTimeout = 60
	}
	// developer 角色兼容默认开启：config 表里没有该 key（含旧库升级）时补默认 true；
	// 显式存了 "false" 则上面解析区已覆盖为 false，这里不再动。
	if _, ok := all["proxy.normalize_developer_role"]; !ok {
		cfg.Proxy.NormalizeDeveloperRole = true
	}
	// reasoning_content 回传缓存默认开启：config 表里没有该 key（含旧库升级）时补默认 true
	// （DeepSeek thinking 模式多轮 tool-calling 要求回传，默认开启兜底上游 400）。
	if _, ok := all["proxy.cache_reasoning_content"]; !ok {
		cfg.Proxy.CacheReasoningContent = true
	}
	// 客户端分析间隔默认 600 秒（10 分钟）；分析开关默认关闭（false）。
	if cfg.Proxy.ClientAnalysisInterval <= 0 {
		cfg.Proxy.ClientAnalysisInterval = 600
	}

	// 加载上游
	upstreams, err := s.ListUpstreams()
	if err != nil {
		return nil, fmt.Errorf("list upstreams: %w", err)
	}
	for _, u := range upstreams {
		up := Upstream{
			Name:     u.Name,
			Type:     u.Type,
			BaseURL:  u.BaseURL,
			APIKey:   u.APIKey,
			Tier:     u.Tier,
			Priority: u.Priority,
			Weight:   u.Weight,
			Enabled:  u.Enabled == 1,
		}
		if u.Models != "" {
			up.Models = ParseModelsString(u.Models)
		}
		if u.ModelMapping != "" {
			json.Unmarshal([]byte(u.ModelMapping), &up.ModelMapping)
		}
		up.RequestOverride = u.RequestOverride
		// 每上游 max_tokens 上限：config 表键 upstream.<name>.max_tokens_cap
		// （不落 upstreams 表，避免动表结构；0=不限制，默认行为不变）
		if v, ok := all["upstream."+u.Name+".max_tokens_cap"]; ok {
			if cap, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && cap > 0 {
				up.MaxTokensCap = cap
			}
		}
		cfg.Upstreams = append(cfg.Upstreams, up)
	}

	// 加载路由规则
	rules, err := s.ListRoutingRules()
	if err != nil {
		return nil, fmt.Errorf("list routing rules: %w", err)
	}
	for _, r := range rules {
		rule := Rule{
			Model:          r.Model,
			Strategy:       r.Strategy,
			Vision:         r.Vision == 1,
			VisionFallback: r.VisionFallback,
		}
		if r.Upstreams != "" {
			// 正常存储是 JSON 数组字符串；兜底兼容历史脏数据（纯逗号串），
			// 避免 json.Unmarshal 失败后 rule.Upstreams 为空导致规则失效（404）。
			rule.Upstreams = ParseModelsString(r.Upstreams)
		}
		cfg.Routing.Rules = append(cfg.Routing.Rules, rule)
	}

	// 加载最佳思考等级配置
	efforts, err := s.ListEffortConfig()
	if err != nil {
		return nil, fmt.Errorf("list effort config: %w", err)
	}
	for _, e := range efforts {
		cfg.Proxy.EffortConfigs = append(cfg.Proxy.EffortConfigs, EffortConfig{
			Model:       e.Model,
			Recommended: e.Recommended,
			Forced:      e.Forced,
		})
	}

	return cfg, nil
}

// ParseModelsString 解析模型列表字符串，兼容两种存储格式：
//  1. JSON 数组：["model1","model2"]
//  2. 逗号分隔：model1, model2
//
// 逗号分隔为主推格式（前端编辑框统一逗号风格）；兼容历史 JSON 数据。
func ParseModelsString(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	// 先试 JSON（历史数据/旧格式）
	if strings.HasPrefix(s, "[") {
		var out []string
		if err := json.Unmarshal([]byte(s), &out); err == nil {
			return out
		}
	}
	// 逗号分隔
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ===== 上游 User-Agent 注入 =====
//
// 转发到上游时，各协议 handler（OpenAI proxy / Gemini / Anthropic / Ollama）
// 构造完上游请求后统一调用 ApplyUpstreamUserAgent 设置 User-Agent。
// 用包级 atomic 而不是把 cfg 传进每个 handler 结构，避免大改签名，
// 并保证配置热重载后立即生效。

// DefaultUpstreamUserAgent 是转发时的默认 User-Agent：pi agent 的真实 UA
// （pi/0.84.1，linux；node v22.22.1；x64，与本机 pi 二进制实测一致）。
const DefaultUpstreamUserAgent = "pi/0.84.1 (linux; node/v22.22.1; x64)"

var upstreamUserAgent atomic.Value // string；空 = 不设置（用 Go 默认 Go-http-client/1.1）

// SetUpstreamUserAgent 设置全局上游 UA（空串 = 不设置）。
func SetUpstreamUserAgent(ua string) { upstreamUserAgent.Store(ua) }

// UpstreamUserAgent 返回当前全局上游 UA。
func UpstreamUserAgent() string {
	if v, ok := upstreamUserAgent.Load().(string); ok {
		return v
	}
	return ""
}

// ApplyUpstreamUserAgent 把全局上游 UA 写到请求头（配置非空时才设置）。
func ApplyUpstreamUserAgent(req *http.Request) {
	if ua := UpstreamUserAgent(); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
}
