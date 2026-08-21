// Package quota 实现按 API key 的模型白名单与模型级配额检查。
//
// 语义（用户拍板）：
//  1. 配额以“模型”为独立核算单元：便宜模型池大、贵模型池小，各模型互不影响，
//     一个模型额度耗尽不阻塞其他模型。
//  2. 组的限额是“组内每人默认”：组 × 模型 配额作为组内每个 key 的默认上限，
//     key 可按模型做例外覆盖（quota_override）。
//  3. 计量单位：原始 token（权重/积分为后续阶段）。
//  4. 用量直接以 request_log 事实表为准（按 api_key.name 快照 + model + 时间窗口
//     SUM(tokens)），无需内存计数器/持久化，状态天然一致。
//
// 窗口：5h 滑动 / 自然周(UTC 周一 00:00 起) / 自然月(UTC 当月 1 日 00:00 起)，
// 任一层超限即拦截（429）。
package quota

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/icefairy/xuanji/internal/store"
)

// WindowLimits 三个时间窗口的 token 上限；0 = 该窗口不限。
type WindowLimits struct {
	FiveH int64 `json:"5h"`
	Week  int64 `json:"week"`
	Month int64 `json:"month"`
}

// HasAny 是否任一窗口设了上限（>0）。
func (w WindowLimits) HasAny() bool {
	return w.FiveH > 0 || w.Week > 0 || w.Month > 0
}

// GroupPolicy 组的策略：模型白名单 + 组内每人默认的模型配额。
type GroupPolicy struct {
	ID      uint
	Name    string
	Allowed []string // 组级模型白名单；空 = 不限
	Quotas  map[string]WindowLimits // model → 组内每人默认配额；"*" = 全模型兜底
}

// TokenPolicy 单个下游 key 的策略（key 显式覆盖 + 组继承参照）。
type TokenPolicy struct {
	Token       string
	Name        string // api_tokens.name 快照（request_log 统计口径）
	GroupID     uint
	OwnAllowed  []string // key 级白名单；nil = 未定义，用组的
	OwnOverride map[string]WindowLimits // key×模型 例外；命中优先于组
	Group       *GroupPolicy
}

// QuotaError 配额拦截的错误（中间件据此写 429/403 响应）。
type QuotaError struct {
	Status  int
	Code    string
	Message string
	Details map[string]interface{}
}

func (e *QuotaError) Error() string { return e.Message }

// Service 持有配置策略快照，供中间件并发读。Load/Refresh 重建快照。
// 用量判定以内存实时计数为准（DB 懒加载做初始值），规避异步批量落库的延迟窗口。
type Service struct {
	mu      sync.RWMutex
	store   *store.Store
	tokens  map[string]*TokenPolicy
	enabled bool // 存在任意策略时启用检查（纯优化：无策略时放行零开销）

	countersMu sync.Mutex
	counters   map[string]*usageCounter // key = name + "\x00" + model

	now func() time.Time // 可注入时钟（测试用）；nil 时用 time.Now
}

// usageCounter 一个 (api_tokens.name, model) 的实时窗口用量。
type usageCounter struct {
	cMu sync.Mutex

	loaded     bool // 是否已用 DB 历史初始化（懒加载一次）
	fiveH      []usageEntry // 近 5h 滑动窗口条目（按时序追加）
	weekTotal  int64
	monthTotal int64
	weekMark  time.Time // 记录所对应的自然周起点
	monthMark time.Time // 记录所对应的自然月起点
}

type usageEntry struct {
	at     time.Time
	tokens int64
}

// New 构建配额服务并加载当前策略。
func New(st *store.Store) *Service {
	s := &Service{store: st, tokens: map[string]*TokenPolicy{}, counters: map[string]*usageCounter{}}
	s.Load()
	return s
}

// SetClock 注入时钟函数（默认 time.Now；仅测试用）。
func (s *Service) SetClock(f func() time.Time) { s.now = f }

func (s *Service) nowUTC() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

// Load 从 DB 全量重建策略快照。
func (s *Service) Load() {
	if s.store == nil {
		return
	}
	groups, err := s.store.ListGroups()
	if err != nil {
		return
	}
	groupMap := make(map[uint]*GroupPolicy)
	for i := range groups {
		g := &groups[i]
		gp := &GroupPolicy{
			ID:      g.ID,
			Name:    g.Name,
			Allowed: parseAllowed(g.AllowedModels),
			Quotas:  map[string]WindowLimits{},
		}
		groupMap[g.ID] = gp
	}
	quotas, err := s.store.ListAllGroupQuotas()
	if err == nil {
		for i := range quotas {
			q := &quotas[i]
			if gp, ok := groupMap[q.GroupID]; ok {
				gp.Quotas[q.Model] = WindowLimits{FiveH: q.Token5H, Week: q.TokenWeek, Month: q.TokenMonth}
			}
		}
	}
	tokens, err := s.store.ListAPITokens()
	if err != nil {
		return
	}
	next := make(map[string]*TokenPolicy, len(tokens))
	any := false
	for i := range tokens {
		t := &tokens[i]
		if t.Key == "" {
			continue
		}
		p := &TokenPolicy{
			Token:       t.Key,
			Name:        t.Name,
			GroupID:     t.GroupID,
			OwnAllowed:  nil,
			OwnOverride: parseOverride(t.QuotaOverride),
		}
		if am := parseAllowed(t.AllowedModels); len(am) > 0 || isExplicitAllowed(t.AllowedModels) {
			p.OwnAllowed = am
		}
		if g, ok := groupMap[t.GroupID]; ok {
			p.Group = g
		}
		// 只有真正配置了限制的 key 才计入启用集合
		if p.Group != nil || len(p.OwnAllowed) > 0 || len(p.OwnOverride) > 0 {
			any = true
		}
		next[t.Key] = p
	}
	s.mu.Lock()
	s.tokens = next
	s.enabled = any
	s.mu.Unlock()
}

// usage counter 工具

func counterKey(name, model string) string { return name + "\x00" + model }

// ensureInit 懒加载：首次触碰某个 (name,model) 时，用 request_log 历史窗口量作为初值。
// 5h 用窗口明细（精确滑动，不折叠避免 double-count），周/月用 SUM（固定窗口无滑动问题）。
func (s *Service) ensureInit(c *usageCounter, name, model string, now time.Time) {
	if c.loaded {
		return
	}
	c.loaded = true
	if s.store != nil {
		if entries, err := s.store.WindowTokenEntries(name, model, now.Add(-5*time.Hour)); err == nil {
			for _, e := range entries {
				c.fiveH = append(c.fiveH, usageEntry{at: e.At, tokens: e.Tokens})
			}
		}
		if v, err := s.store.WindowTokenSum(name, model, WeekStart(now)); err == nil {
			c.weekTotal = v
		}
		if v, err := s.store.WindowTokenSum(name, model, MonthStart(now)); err == nil {
			c.monthTotal = v
		}
	}
	c.weekMark = WeekStart(now)
	c.monthMark = MonthStart(now)
}

// AddUsage 记录一次用量（成功转发后调用；由 Recorder usageHook 注入）。
func (s *Service) AddUsage(name, model string, tokens int64) {
	if tokens <= 0 || name == "" {
		return
	}
	now := s.nowUTC()
	s.countersMu.Lock()
	c := s.counters[counterKey(name, model)]
	if c == nil {
		c = &usageCounter{}
		s.counters[counterKey(name, model)] = c
	}
	// 注意：AddUsage 也称 ensureInit，但 DB 记录由本钩子投递、尚未落库，
	// 懒加载会把它算作“历史”——但时间戳也在窗口内，量一致，不双计。
	c.cMu.Lock()
	if !c.loaded {
		s.ensureInit(c, name, model, now)
	}
	s.countersMu.Unlock()

	c.fiveH = append(c.fiveH, usageEntry{at: now, tokens: tokens})
	// 跨周/跨月自动重置
	if wk := WeekStart(now); !wk.Equal(c.weekMark) {
		c.weekTotal = 0
		c.weekMark = wk
	}
	if mk := MonthStart(now); !mk.Equal(c.monthMark) {
		c.monthTotal = 0
		c.monthMark = mk
	}
	c.weekTotal += tokens
	c.monthTotal += tokens
	c.cMu.Unlock()
}

// current 取某个 (name,model) 的三窗口当前用量（滑动窗口自动裁剪过期 5h 条目）。
func (s *Service) current(name, model string, now time.Time) WindowLimits {
	s.countersMu.Lock()
	c := s.counters[counterKey(name, model)]
	if c == nil {
		c = &usageCounter{}
		s.counters[counterKey(name, model)] = c
		s.countersMu.Unlock()
	} else {
		s.countersMu.Unlock()
	}
	c.cMu.Lock()
	defer c.cMu.Unlock()
	if !c.loaded {
		s.ensureInit(c, name, model, now)
	}
	// 裁剪 5h 滑动窗口
	cut := now.Add(-5 * time.Hour)
	kept := c.fiveH[:0]
	for _, e := range c.fiveH {
		if e.at.After(cut) || e.at.Equal(cut) {
			kept = append(kept, e)
		}
	}
	c.fiveH = kept
	var total int64
	for _, e := range c.fiveH {
		total += e.tokens
	}
	return WindowLimits{FiveH: total, Week: c.weekTotal, Month: c.monthTotal}
}

// Refresh 别名 Load（管理端改动后调用）。
func (s *Service) Refresh() { s.Load() }

// Enabled 是否有任何配额策略生效（供状态页/测试）。
func (s *Service) Enabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.enabled
}

func (s *Service) policy(token string) *TokenPolicy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tokens[token]
}

// WeekStart 返回 now 所在自然周的周一 00:00:00（UTC）。
func WeekStart(now time.Time) time.Time {
	d := now.UTC()
	// time.Weekday: Sunday=0
	offset := (int(d.Weekday()) + 6) % 7 // 周一=0
	start := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
	return start.AddDate(0, 0, -offset)
}

// MonthStart 返回 now 所在自然月的 1 日 00:00:00（UTC）。
func MonthStart(now time.Time) time.Time {
	d := now.UTC()
	return time.Date(d.Year(), d.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// Check 对一次请求做白名单 + 配额检查。model 为空时不做配额校验（返回 nil）。
//
// 语义：配置了（key/组 × model）配额的模型 → 隐含允许，走窗口用量检查；
// 未配置配额的模型 → 仅受显式白名单约束（白名单内可用但不限量）。
func (s *Service) Check(token, model string) *QuotaError {
	if token == "" || model == "" {
		return nil
	}
	p := s.policy(token)
	if p == nil {
		return nil // 该 key 未配置任何策略 → 放行
	}
	// 1. 限额选择：key×model → key×"*" → 组×model → 组×"*"
	lim, ok, limSrc := pickLimit(p, model)
	if !ok || !lim.HasAny() {
		// 该模型无（有效）配额 → 只用显式白名单过滤
		allowed := p.OwnAllowed
		src := "key"
		if allowed == nil {
			allowed = groupAllowed(p.Group)
			src = "group"
		}
		if allowed != nil && !containsModel(allowed, model) {
			return &QuotaError{
				Status:  403,
				Code:    "model_not_allowed",
				Message: fmt.Sprintf("模型 %s 不在%s白名单内", model, src),
				Details: map[string]interface{}{"model": model, "allowed": allowed},
			}
		}
		return nil
	}
	// 2. 有配额 → 隐含允许，窗口用量检查（内存实时计数 + DB 懒加载初值）
	now := s.nowUTC()
	used := s.current(p.Name, model, now)
	// 3. 任一超限即拦截
	var exceeded string
	switch {
	case lim.FiveH > 0 && used.FiveH >= lim.FiveH:
		exceeded = "5h"
	case lim.Week > 0 && used.Week >= lim.Week:
		exceeded = "week"
	case lim.Month > 0 && used.Month >= lim.Month:
		exceeded = "month"
	}
	if exceeded == "" {
		return nil
	}
	return &QuotaError{
		Status:  429,
		Code:    "quota_exceeded",
		Message: fmt.Sprintf("模型 %s 的 %s 配额已用尽", model, exceeded),
		Details: map[string]interface{}{
			"model":        model,
			"quota_source": limSrc,
			"used":         used,
			"limit":        lim,
			"exceeded":     exceeded,
		},
	}
}

// pickLimit 按优先级取模型的额度：key×model → key×"*" → 组×model → 组×"*"。
func pickLimit(p *TokenPolicy, model string) (lim WindowLimits, ok bool, src string) {
	if l, ok2 := p.OwnOverride[model]; ok2 {
		return l, true, "key:" + model
	}
	if p.OwnOverride != nil {
		if l, ok2 := p.OwnOverride["*"]; ok2 {
			return l, true, "key:*"
		}
	}
	if p.Group != nil {
		if q, ok2 := p.Group.Quotas[model]; ok2 {
			return q, true, "group:" + model
		}
		if q, ok2 := p.Group.Quotas["*"]; ok2 {
			return q, true, "group:*"
		}
	}
	return WindowLimits{}, false, ""
}

// groupAllowed 组白名单；无组返回 nil（不限）。
func groupAllowed(g *GroupPolicy) []string {
	if g == nil {
		return nil
	}
	return g.Allowed
}

// containsModel 判断 model 是否命中白名单（支持 "*" 通配全模型）。
func containsModel(allowed []string, model string) bool {
	for _, m := range allowed {
		if m == "*" || m == model {
			return true
		}
	}
	return false
}

// parseAllowed 解析白名单 JSON 数组；返回 nil 表示未配置（继承/不限）。
func parseAllowed(s string) []string {
	ss := strings.TrimSpace(s)
	if ss == "" || ss == "[]" || ss == "null" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(ss), &out); err != nil {
		return nil
	}
	names := make([]string, 0, len(out))
	for _, v := range out {
		if strings.TrimSpace(v) != "" {
			names = append(names, strings.TrimSpace(v))
		}
	}
	sort.Strings(names)
	return names
}

// isExplicitAllowed 判断 allowed_models 是否为显式白名单（如 "[]" 表示 key 级为空=禁止全部）。
func isExplicitAllowed(s string) bool {
	return strings.TrimSpace(s) != "" && strings.TrimSpace(s) != "[]"
}

// parseOverride 解析 key×模型 例外配额 JSON：{model:{"5h":..,"week":..,"month":..}}。
func parseOverride(s string) map[string]WindowLimits {
	ss := strings.TrimSpace(s)
	if ss == "" || ss == "{}" || ss == "null" {
		return nil
	}
	var raw map[string]struct {
		FiveH int64 `json:"5h"`
		Week  int64 `json:"week"`
		Month int64 `json:"month"`
	}
	if err := json.Unmarshal([]byte(ss), &raw); err != nil {
		return nil
	}
	out := make(map[string]WindowLimits, len(raw))
	for m, v := range raw {
		out[m] = WindowLimits{FiveH: v.FiveH, Week: v.Week, Month: v.Month}
	}
	return out
}
