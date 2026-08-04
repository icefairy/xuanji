// Package store 提供 SQLite 持久化与异步写入。
package store

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，注册 "sqlite" driver 名
)

// Record 是一次转发请求的指标记录。
type Record struct {
	Timestamp        time.Time
	Upstream         string // 实际转发到的上游名
	Model            string // 客户端请求的模型名
	Endpoint         string // chat / images / audio / embed / claude / generate
	Status           int    // HTTP 状态码
	DurationMS       int64  // 转发耗时毫秒
	PromptTokens         int64  // 输入 token 数
	CompletionTokens     int64  // 输出 token 数
	Tokens               int64  // 总 token 数 = PromptTokens + CompletionTokens
	APIKey               string // 下游 API Key 名称（api_tokens.name，用于按 Key 统计）
	PromptCacheHitTokens  int64  // 上游前缀缓存命中 token 数（DeepSeek prompt_cache_hit_tokens）
	PromptCacheMissTokens int64  // 上游前缀缓存未命中 token 数（DeepSeek prompt_cache_miss_tokens）
}

// UpstreamRow 是 upstreams 表的行映射。
type UpstreamRow struct {
	ID           uint   `json:"id"`
	Name         string `json:"name"`
	Type         string `json:"type"`
	BaseURL      string `json:"base_url"`
	APIKey       string `json:"api_key"`
	Tier         string `json:"tier"`
	Priority     int    `json:"priority"`
	Weight       int    `json:"weight"`
	Models       string `json:"models"`
	ModelMapping string `json:"model_mapping"`
	Enabled      int    `json:"enabled"` // 1=启用 0=禁用（禁用的不参与转发路由）
	// EnabledPtr 区分 JSON body 中 enabled 字段"未传"(nil) 与"显式传 0/1"。
	// UpdateUpstream 用它避免未传时误禁用上游。
	EnabledPtr *int `json:"-"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// RoutingRuleRow 是 routing_rules 表的行映射。
type RoutingRuleRow struct {
	ID        uint   `json:"id"`
	Model     string `json:"model"`
	Strategy  string `json:"strategy"`
	Upstreams string `json:"upstreams"` // JSON 数组
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// ConfigRow 是 config 表的行映射（key-value 存储）。
type ConfigRow struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// APIToken 是下游 API Key（api_tokens 表）的行映射。
type APIToken struct {
	ID        uint   `json:"id"`
	Name      string `json:"name"`      // 用途备注，如 "Claude Code" / "Hermes"
	Key       string `json:"key"`       // 下游调用时用的 Bearer token
	Enabled   bool   `json:"enabled"`   // 是否启用
	Remark    string `json:"remark"`    // 备注
	CreatedAt string `json:"created_at"`
}

// User 是管理端用户（users 表）的行映射。
type User struct {
	ID           uint   `json:"id"`
	Username     string `json:"username"`
	PasswordHash string `json:"-"`
	CreatedAt    string `json:"created_at"`
}

// Discount 是渠道优惠时段（discounts 表）的行映射。
// 例：硅基流动 23:00-07:00 全部模型 5 折 → {Upstream:"硅基流动", ModelPattern:"*", StartTime:"23:00", EndTime:"07:00", Discount:0.5}
type Discount struct {
	ID           uint    `json:"id"`
	Upstream     string  `json:"upstream"`      // 上游名称
	ModelPattern string  `json:"model_pattern"` // 适用模型，* = 全部，或用逗号分隔具体模型
	StartTime    string  `json:"start_time"`    // HH:MM 开始（如 23:00）
	EndTime      string  `json:"end_time"`      // HH:MM 结束（如 07:00，支持跨天）
	Discount     float64 `json:"discount"`      // 折扣率，0.5=半价，0.8=8折，1=无折扣
	Note         string  `json:"note"`          // 备注
	CreatedAt    string  `json:"created_at"`
}

// Store 封装 SQLite 连接与表结构。
type Store struct {
	db *sql.DB
}

// Open 打开（或创建）SQLite 数据库文件，启用 WAL，建表。
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// WAL 模式：读不阻塞写，写不阻塞读，适合网关并发写入场景。
	if _, err := db.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		db.Close()
		return nil, err
	}
	// busy_timeout：写锁冲突时等待而非立即报错。
	if _, err := db.Exec(`PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, err
	}
	// 页缓存 + mmap：整个库（~百 KB）热数据常驻内存，查询无需磁盘 IO。
	// cache_size=-20000 = 20MB；mmap_size=64MB 让读走内存映射。
	if _, err := db.Exec(`PRAGMA cache_size=-20000;`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA mmap_size=67108864;`); err != nil {
		db.Close()
		return nil, err
	}
	// synchronous=NORMAL：WAL 模式下崩溃最多丢最近提交，换取批量写入吞吐。
	if _, err := db.Exec(`PRAGMA synchronous=NORMAL;`); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// init 建表并创建查询索引。
func (s *Store) init() error {
	const schema = `
	CREATE TABLE IF NOT EXISTS request_log (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		ts          TEXT    NOT NULL,              -- RFC3339
		upstream    TEXT    NOT NULL,
		model       TEXT    NOT NULL DEFAULT '',
		endpoint    TEXT    NOT NULL,
		status      INTEGER NOT NULL,
		duration_ms INTEGER NOT NULL,
		tokens      INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_request_log_ts ON request_log(ts);
	CREATE INDEX IF NOT EXISTS idx_request_log_upstream ON request_log(upstream);

	-- 定时探测结果（健康检查）。逐条记录，metrics 按时间范围聚合；
	-- 与 request_log 对称，重启后保留历史，健康度不因重启归零。
	CREATE TABLE IF NOT EXISTS health_probe_log (
		id       INTEGER PRIMARY KEY AUTOINCREMENT,
		ts       TEXT    NOT NULL,              -- RFC3339
		upstream TEXT    NOT NULL,
		ok       INTEGER NOT NULL               -- 1=成功 0=失败
	);
	CREATE INDEX IF NOT EXISTS idx_health_probe_ts ON health_probe_log(ts);
	CREATE INDEX IF NOT EXISTS idx_health_probe_upstream ON health_probe_log(upstream);

	CREATE TABLE IF NOT EXISTS upstreams (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		name         TEXT    NOT NULL UNIQUE,
		type         TEXT    NOT NULL DEFAULT '',
		base_url     TEXT    NOT NULL DEFAULT '',
		api_key      TEXT    NOT NULL DEFAULT '',
		tier         TEXT    NOT NULL DEFAULT '',
		priority     INTEGER NOT NULL DEFAULT 0,
		weight       INTEGER NOT NULL DEFAULT 0,
		models       TEXT    NOT NULL DEFAULT '[]',
		model_mapping TEXT   NOT NULL DEFAULT '{}',
		created_at   TEXT    NOT NULL DEFAULT (datetime('now')),
		updated_at   TEXT    NOT NULL DEFAULT (datetime('now'))
	);

	CREATE TABLE IF NOT EXISTS routing_rules (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		model      TEXT    NOT NULL UNIQUE,
		strategy   TEXT    NOT NULL DEFAULT '',
		upstreams  TEXT    NOT NULL DEFAULT '[]',
		created_at TEXT    NOT NULL DEFAULT (datetime('now')),
		updated_at TEXT    NOT NULL DEFAULT (datetime('now'))
	);

	CREATE TABLE IF NOT EXISTS config (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS effort_config (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		model       TEXT    NOT NULL UNIQUE,
		recommended TEXT    NOT NULL DEFAULT '',
		forced      TEXT    NOT NULL DEFAULT '',
		created_at  TEXT    NOT NULL DEFAULT (datetime('now')),
		updated_at  TEXT    NOT NULL DEFAULT (datetime('now'))
	);

	CREATE TABLE IF NOT EXISTS api_tokens (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		name       TEXT    NOT NULL DEFAULT '',
		key        TEXT    NOT NULL UNIQUE,
		enabled    INTEGER NOT NULL DEFAULT 1,
		remark     TEXT    NOT NULL DEFAULT '',
		created_at TEXT    NOT NULL DEFAULT (datetime('now'))
	);

	CREATE TABLE IF NOT EXISTS users (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		username      TEXT    NOT NULL UNIQUE,
		password_hash TEXT    NOT NULL DEFAULT '',
		created_at    TEXT    NOT NULL DEFAULT (datetime('now'))
	);

	CREATE TABLE IF NOT EXISTS discounts (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		upstream      TEXT    NOT NULL DEFAULT '',
		model_pattern TEXT    NOT NULL DEFAULT '*',
		start_time    TEXT    NOT NULL DEFAULT '00:00',
		end_time      TEXT    NOT NULL DEFAULT '23:59',
		discount      REAL    NOT NULL DEFAULT 1.0,
		note          TEXT    NOT NULL DEFAULT '',
		created_at    TEXT    NOT NULL DEFAULT (datetime('now'))
	);
	`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	// 迁移：新增 prompt_tokens / completion_tokens 列（已存在的忽略错误）
	for _, col := range []string{"prompt_tokens", "completion_tokens"} {
		s.db.Exec("ALTER TABLE request_log ADD COLUMN " + col + " INTEGER NOT NULL DEFAULT 0")
	}
	// 迁移：新增前缀缓存命中统计列（DeepSeek prompt_cache_hit/miss_tokens）
	for _, col := range []string{"prompt_cache_hit_tokens", "prompt_cache_miss_tokens"} {
		s.db.Exec("ALTER TABLE request_log ADD COLUMN " + col + " INTEGER NOT NULL DEFAULT 0")
	}
	// 迁移：request_log 加 api_key 列（按下游 Key 统计）
	s.db.Exec("ALTER TABLE request_log ADD COLUMN api_key TEXT NOT NULL DEFAULT ''")
	s.db.Exec("CREATE INDEX IF NOT EXISTS idx_request_log_apikey ON request_log(api_key)")
	// 迁移：upstreams 加 enabled 列（禁用/启用）
	s.db.Exec("ALTER TABLE upstreams ADD COLUMN enabled INTEGER NOT NULL DEFAULT 1")
	return nil
}

// Close 关闭数据库连接。
func (s *Store) Close() error {
	return s.db.Close()
}

// DB 暴露 *sql.DB 供 admin 查询统计。
func (s *Store) DB() *sql.DB { return s.db }

// Insert 单条插入一条请求记录。
func (s *Store) Insert(rec Record) error {
	_, err := s.db.Exec(
		`INSERT INTO request_log (ts, upstream, model, endpoint, status, duration_ms, tokens, prompt_tokens, completion_tokens, api_key, prompt_cache_hit_tokens, prompt_cache_miss_tokens)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Timestamp.UTC().Format(time.RFC3339), rec.Upstream, rec.Model, rec.Endpoint,
		rec.Status, rec.DurationMS, rec.Tokens, rec.PromptTokens, rec.CompletionTokens, rec.APIKey,
		rec.PromptCacheHitTokens, rec.PromptCacheMissTokens,
	)
	return err
}

// RecordProbe 记录一次定时探测结果（健康检查）。ok=true 表示探测成功。
// 逐条落库，metrics 按时间范围聚合；重启后历史保留，健康度不归零。
func (s *Store) RecordProbe(upstream string, ok bool, at time.Time) error {
	okInt := 0
	if ok {
		okInt = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO health_probe_log (ts, upstream, ok) VALUES (?, ?, ?)`,
		at.UTC().Format(time.RFC3339), upstream, okInt,
	)
	return err
}

// ProbeStats 返回指定上游在 since（RFC3339，含边界）之后的探测统计。
// 未传 since（空串）时统计全部。返回成功/失败次数。
func (s *Store) ProbeStats(upstream, since string) (success, fail int64) {
	var q string
	var args []any
	if since != "" {
		q = `SELECT COALESCE(SUM(CASE WHEN ok=1 THEN 1 ELSE 0 END),0),
		            COALESCE(SUM(CASE WHEN ok=0 THEN 1 ELSE 0 END),0)
		     FROM health_probe_log WHERE upstream=? AND ts >= ?`
		args = []any{upstream, since}
	} else {
		q = `SELECT COALESCE(SUM(CASE WHEN ok=1 THEN 1 ELSE 0 END),0),
		            COALESCE(SUM(CASE WHEN ok=0 THEN 1 ELSE 0 END),0)
		     FROM health_probe_log WHERE upstream=?`
		args = []any{upstream}
	}
	_ = s.db.QueryRow(q, args...).Scan(&success, &fail)
	return success, fail
}

// APIKeyRow 是按下游 Key 聚合的统计行。
type APIKeyRow struct {
	Name         string
	Requests     int64
	Successes    int64
	AvgLatencyMS float64
	TotalTokens  int64
}

// MetricsByAPIKey 返回按下游 API Key 聚合的统计（用于"哪些程序用得多"）。
// since 为空串时统计全部。按请求数降序。
func (s *Store) MetricsByAPIKey(since string) []APIKeyRow {
	q := `SELECT COALESCE(NULLIF(api_key, ''), (SELECT name FROM api_tokens WHERE enabled=1 ORDER BY id LIMIT 1), '(未标识)') as name,
	              COUNT(*) as requests,
	              COALESCE(SUM(CASE WHEN status < 400 THEN 1 ELSE 0 END), 0) as successes,
	              COALESCE(AVG(duration_ms), 0) as avg_ms,
	              COALESCE(SUM(tokens), 0) as tokens
	       FROM request_log`
	var args []any
	if since != "" {
		q += ` WHERE ts >= ?`
		args = append(args, since)
	}
	q += ` GROUP BY name ORDER BY tokens DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []APIKeyRow
	for rows.Next() {
		var r APIKeyRow
		if err := rows.Scan(&r.Name, &r.Requests, &r.Successes, &r.AvgLatencyMS, &r.TotalTokens); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out
}

// InsertBatch 在单个事务内批量插入多条记录。
func (s *Store) InsertBatch(recs []Record) error {
	if len(recs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		`INSERT INTO request_log (ts, upstream, model, endpoint, status, duration_ms, tokens, prompt_tokens, completion_tokens, api_key, prompt_cache_hit_tokens, prompt_cache_miss_tokens)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, rec := range recs {
		if _, err := stmt.Exec(
			rec.Timestamp.UTC().Format(time.RFC3339), rec.Upstream, rec.Model, rec.Endpoint,
			rec.Status, rec.DurationMS, rec.Tokens, rec.PromptTokens, rec.CompletionTokens, rec.APIKey,
			rec.PromptCacheHitTokens, rec.PromptCacheMissTokens,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// recorder 默认批量参数。
const (
	defaultBatchSize   = 100
	defaultBatchPeriod = 2 * time.Second
)

// Recorder 提供异步批量写入：请求记录进 channel，后台 goroutine 定时批量 flush。
// 不阻塞转发路径；Close 时 flush 剩余并关闭。
type Recorder struct {
	store     *Store
	ch        chan Record
	wg        sync.WaitGroup
	quit      chan struct{}
	closeOnce sync.Once
	dropOnce  sync.Once
	log       *slog.Logger
}

// NewRecorder 创建并启动后台写入协程。batchInterval 默认 2s 或 batchSize 达到 100 即刷。
func NewRecorder(s *Store) *Recorder {
	return newRecorder(s, defaultBatchSize, defaultBatchPeriod)
}

// newRecorder 是 NewRecorder 的可参数化版本，供测试控制缓冲与刷盘节奏。
func newRecorder(s *Store, batchSize int, batchPeriod time.Duration) *Recorder {
	r := &Recorder{
		store: s,
		ch:    make(chan Record, batchSize),
		quit:  make(chan struct{}),
		log:   slog.Default(),
	}
	r.wg.Add(1)
	go r.loop(batchSize, batchPeriod)
	return r
}

// loop 是后台批量写入协程：定时或积满 batchSize 时批量落库。
func (r *Recorder) loop(batchSize int, batchPeriod time.Duration) {
	defer r.wg.Done()

	buf := make([]Record, 0, batchSize)
	ticker := time.NewTicker(batchPeriod)
	defer ticker.Stop()

	flush := func() {
		if len(buf) == 0 {
			return
		}
		if err := r.store.InsertBatch(buf); err != nil {
			r.log.Warn("store: batch insert failed", "error", err, "count", len(buf))
		}
		buf = buf[:0]
	}

	for {
		select {
		case <-r.quit:
			// 关闭：把 channel 里剩余记录也 flush 掉
			for {
				select {
				case rec := <-r.ch:
					buf = append(buf, rec)
				default:
					flush()
					return
				}
			}
		case rec := <-r.ch:
			buf = append(buf, rec)
			if len(buf) >= batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// Record 非阻塞记录一条指标：channel 满时丢弃（记录量超限不拖垮网关）。
func (r *Recorder) Record(rec Record) {
	select {
	case r.ch <- rec:
	default:
		r.dropOnce.Do(func() {
			r.log.Warn("store: recorder channel full, dropping records")
		})
	}
}

// Store 暴露内部 store 引用，供 admin 查询 metrics。
func (r *Recorder) Store() *Store { return r.store }

// Close flush 剩余记录并关闭后台协程；可安全重复调用。
func (r *Recorder) Close() {
	r.closeOnce.Do(func() {
		close(r.quit)
		r.wg.Wait()
	})
}

// ===== Upstreams CRUD =====

// ListUpstreams 返回所有上游。
func (s *Store) ListUpstreams() ([]UpstreamRow, error) {
	rows, err := s.db.Query(`SELECT id, name, type, base_url, api_key, tier, priority, weight, models, model_mapping, enabled, created_at, updated_at FROM upstreams ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []UpstreamRow
	for rows.Next() {
		var u UpstreamRow
		if err := rows.Scan(&u.ID, &u.Name, &u.Type, &u.BaseURL, &u.APIKey, &u.Tier, &u.Priority, &u.Weight, &u.Models, &u.ModelMapping, &u.Enabled, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// GetUpstream 按名称查询上游。
func (s *Store) GetUpstream(name string) (*UpstreamRow, error) {
	var u UpstreamRow
	err := s.db.QueryRow(`SELECT id, name, type, base_url, api_key, tier, priority, weight, models, model_mapping, enabled, created_at, updated_at FROM upstreams WHERE name = ?`, name).
		Scan(&u.ID, &u.Name, &u.Type, &u.BaseURL, &u.APIKey, &u.Tier, &u.Priority, &u.Weight, &u.Models, &u.ModelMapping, &u.Enabled, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// CreateUpstream 创建上游。
// CreateUpstream 创建上游。enabled 未传（nil）时默认启用（1）。
func (s *Store) CreateUpstream(u *UpstreamRow) error {
	enabled := 1
	if u.EnabledPtr != nil {
		enabled = *u.EnabledPtr
	}
	_, err := s.db.Exec(
		`INSERT INTO upstreams (name, type, base_url, api_key, tier, priority, weight, models, model_mapping, enabled) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.Name, u.Type, u.BaseURL, u.APIKey, u.Tier, u.Priority, u.Weight, u.Models, u.ModelMapping, enabled,
	)
	return err
}

// UpdateUpstream 更新上游（按名称匹配）。
// enabled 用指针区分"未传"（nil=保持原值）与"显式传 0/1"，
// 避免前端编辑表单不传 enabled 时误把上游禁用。
func (s *Store) UpdateUpstream(name string, u *UpstreamRow) error {
	var enabledExpr string
	var args []any
	args = append(args, u.Name, u.Type, u.BaseURL, u.APIKey, u.Tier, u.Priority, u.Weight, u.Models, u.ModelMapping)
	if u.EnabledPtr != nil {
		enabledExpr = ", enabled=?"
		args = append(args, *u.EnabledPtr)
	}
	args = append(args, name)
	_, err := s.db.Exec(
		`UPDATE upstreams SET name=?, type=?, base_url=?, api_key=?, tier=?, priority=?, weight=?, models=?, model_mapping=?`+enabledExpr+`, updated_at=datetime('now') WHERE name=?`,
		args...,
	)
	return err
}

// SetUpstreamEnabled 切换上游启用状态（1/0），返回新状态。
func (s *Store) SetUpstreamEnabled(name string, enabled int) error {
	if enabled != 0 {
		enabled = 1
	}
	_, err := s.db.Exec(`UPDATE upstreams SET enabled=?, updated_at=datetime('now') WHERE name=?`, enabled, name)
	return err
}

// DeleteUpstream 删除上游，并同步清理所有路由规则中对该上游的引用
// （避免规则残留已不存在的上游名，导致"删不掉、页面仍显示"的孤儿引用）。
func (s *Store) DeleteUpstream(name string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM upstreams WHERE name = ?`, name); err != nil {
		return err
	}
	// 清理所有规则 JSON 里的引用
	rows, err := tx.Query(`SELECT id, upstreams FROM routing_rules`)
	if err != nil {
		return err
	}
	type ruleRow struct {
		id       int64
		upstreams string
	}
	var toUpdate []ruleRow
	for rows.Next() {
		var rr ruleRow
		if err := rows.Scan(&rr.id, &rr.upstreams); err != nil {
			continue
		}
		var ups []string
		if err := json.Unmarshal([]byte(rr.upstreams), &ups); err != nil {
			continue
		}
		filtered := ups[:0]
		changed := false
		for _, u := range ups {
			if u == name {
				changed = true
				continue
			}
			filtered = append(filtered, u)
		}
		if changed {
			toUpdate = append(toUpdate, ruleRow{rr.id, mustJSON(filtered)})
		}
	}
	rows.Close()
	for _, rr := range toUpdate {
		if _, err := tx.Exec(`UPDATE routing_rules SET upstreams=?, updated_at=datetime('now') WHERE id=?`, rr.upstreams, rr.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// mustJSON 将 []string 编码为 JSON 字符串（忽略错误，调用方保证可序列化）。
func mustJSON(v []string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ===== Routing Rules CRUD =====

// ListRoutingRules 返回所有路由规则。
func (s *Store) ListRoutingRules() ([]RoutingRuleRow, error) {
	rows, err := s.db.Query(`SELECT id, model, strategy, upstreams, created_at, updated_at FROM routing_rules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RoutingRuleRow
	for rows.Next() {
		var r RoutingRuleRow
		if err := rows.Scan(&r.ID, &r.Model, &r.Strategy, &r.Upstreams, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRoutingRule 按 model 查询。
func (s *Store) GetRoutingRule(model string) (*RoutingRuleRow, error) {
	var r RoutingRuleRow
	err := s.db.QueryRow(`SELECT id, model, strategy, upstreams, created_at, updated_at FROM routing_rules WHERE model = ?`, model).
		Scan(&r.ID, &r.Model, &r.Strategy, &r.Upstreams, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// CreateRoutingRule 创建路由规则。
func (s *Store) CreateRoutingRule(r *RoutingRuleRow) error {
	_, err := s.db.Exec(
		`INSERT INTO routing_rules (model, strategy, upstreams) VALUES (?, ?, ?)`,
		r.Model, r.Strategy, r.Upstreams,
	)
	return err
}

// UpdateRoutingRule 更新路由规则（按 model 匹配）。
func (s *Store) UpdateRoutingRule(model string, r *RoutingRuleRow) error {
	_, err := s.db.Exec(
		`UPDATE routing_rules SET model=?, strategy=?, upstreams=?, updated_at=datetime('now') WHERE model=?`,
		r.Model, r.Strategy, r.Upstreams, model,
	)
	return err
}

// DeleteRoutingRule 删除路由规则。
func (s *Store) DeleteRoutingRule(model string) error {
	_, err := s.db.Exec(`DELETE FROM routing_rules WHERE model = ?`, model)
	return err
}

// ===== EffortConfig（最佳思考等级）=====

// EffortConfigRow 是 effort_config 表的行映射。
type EffortConfigRow struct {
	ID           uint   `json:"id"`
	Model        string `json:"model"`        // 模型匹配 pattern（支持 * 通配）
	Recommended  string `json:"recommended"`  // 推荐思考等级（客户端未传时自动补）
	Forced       string `json:"forced"`       // 强制思考等级（覆盖客户端传的）
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// ListEffortConfig 返回所有最佳思考等级配置。
func (s *Store) ListEffortConfig() ([]EffortConfigRow, error) {
	rows, err := s.db.Query(`SELECT id, model, recommended, forced, created_at, updated_at FROM effort_config ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []EffortConfigRow
	for rows.Next() {
		var r EffortConfigRow
		if err := rows.Scan(&r.ID, &r.Model, &r.Recommended, &r.Forced, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CreateEffortConfig 创建最佳思考等级配置。
func (s *Store) CreateEffortConfig(r *EffortConfigRow) error {
	_, err := s.db.Exec(
		`INSERT INTO effort_config (model, recommended, forced) VALUES (?, ?, ?)`,
		r.Model, r.Recommended, r.Forced,
	)
	return err
}

// UpdateEffortConfig 更新最佳思考等级配置（按 model 匹配）。
func (s *Store) UpdateEffortConfig(model string, r *EffortConfigRow) error {
	_, err := s.db.Exec(
		`UPDATE effort_config SET model=?, recommended=?, forced=?, updated_at=datetime('now') WHERE model=?`,
		r.Model, r.Recommended, r.Forced, model,
	)
	return err
}

// DeleteEffortConfig 删除最佳思考等级配置。
func (s *Store) DeleteEffortConfig(model string) error {
	_, err := s.db.Exec(`DELETE FROM effort_config WHERE model = ?`, model)
	return err
}

// ===== Config key-value =====

// GetConfig 读取配置值，不存在返回空字符串。
func (s *Store) GetConfig(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM config WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// SetConfig 写入配置值，key 已存在则更新。
func (s *Store) SetConfig(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO config (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	return err
}

// GetAllConfig 读取所有配置，返回 key-value 映射。
func (s *Store) GetAllConfig() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM config`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SeedDefaults 写入默认配置（仅当 config 表为空时，不覆盖已有数据）。
func (s *Store) SeedDefaults() error {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM config`).Scan(&count); err != nil {
		return err
	}
	// defaults 是 SeedDefaults 的基准表。首次启动全量插入；已有数据时用 INSERT OR IGNORE
	// 补缺失配置项（旧版本没有的新 key），不覆盖用户已改的值。
	// 新增 key → INSERT OR IGNORE 生效；已存在 key → 保持用户值。
	// 注意：要"升级"已有 key 的默认值（如 fast_fail_minutes 60→5），需手动 DB 更新，
	// 或通过 admin API 重置。此处只做补缺，不覆盖。
	defaults := map[string]string{
		"server.port":                   "8787",
		"retry.max_retries":             "3",
		"retry.retry_statuses":          "429,500,502,503,504",
		"retry.retry_keywords":          "套餐用完了,余额不足,quota,rate limit",
		"retry.fast_fail_minutes":        "5",
		"retry.fast_fail_probe_minutes":   "5",
		"retry.upstream_timeout":        "60",
		// 视频透传（默认关）
		"proxy.video_pass_through": "false",
		// per-key 冷却（商汤默认 1 秒）
		"proxy.cooldown_seconds":      "1",
		"proxy.cooldown_upstreams":    "[\"商汤\"]",
		// 最佳思考等级（默认关）
		"proxy.auto_best_effort":  "false",
		"proxy.force_best_effort": "false",
	}
	if count == 0 {
		// 首次启动：全量插入
		for k, v := range defaults {
			if _, err := s.db.Exec(`INSERT INTO config (key, value) VALUES (?, ?)`, k, v); err != nil {
				return err
			}
		}
		return nil
	}
	// 已有数据：只补缺失的 key（INSERT OR IGNORE 自动跳过已存在）
	for k, v := range defaults {
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO config (key, value) VALUES (?, ?)`, k, v); err != nil {
			return err
		}
	}
	return nil
}

// ===== API Token（下游 API Key）=====

// CreateAPIToken 创建下游 API Key。
func (s *Store) CreateAPIToken(name, key, remark string) (*APIToken, error) {
	res, err := s.db.Exec(
		`INSERT INTO api_tokens (name, key, remark) VALUES (?, ?, ?)`,
		name, key, remark,
	)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &APIToken{ID: uint(id), Name: name, Key: key, Enabled: true, Remark: remark, CreatedAt: time.Now().Format(time.RFC3339)}, nil
}

// ListAPITokens 列出所有下游 API Key。
func (s *Store) ListAPITokens() ([]APIToken, error) {
	rows, err := s.db.Query(`SELECT id, name, key, enabled, remark, created_at FROM api_tokens ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []APIToken
	for rows.Next() {
		var t APIToken
		var enabled int
		if err := rows.Scan(&t.ID, &t.Name, &t.Key, &enabled, &t.Remark, &t.CreatedAt); err != nil {
			return nil, err
		}
		t.Enabled = enabled != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteAPIToken 删除下游 API Key（按 id）。
func (s *Store) DeleteAPIToken(id uint) error {
	_, err := s.db.Exec(`DELETE FROM api_tokens WHERE id = ?`, id)
	return err
}

// SetAPITokenEnabled 启用/禁用下游 API Key。
func (s *Store) SetAPITokenEnabled(id uint, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := s.db.Exec(`UPDATE api_tokens SET enabled = ? WHERE id = ?`, v, id)
	return err
}

// APITokenExists 检查 key 是否已存在且启用（避免重复 + 鉴权用）。
func (s *Store) APITokenExists(key string) bool {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM api_tokens WHERE key = ? AND enabled = 1`, key).Scan(&n)
	return err == nil && n > 0
}

// AllEnabledTokenKeys 返回所有启用的下游 key（鉴权用）。
func (s *Store) AllEnabledTokenKeys() map[string]bool {
	out := make(map[string]bool)
	rows, err := s.db.Query(`SELECT key FROM api_tokens WHERE enabled = 1`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err == nil && k != "" {
			out[k] = true
		}
	}
	return out
}

// AllEnabledTokenNames 返回启用的下游 key → 名称映射（统计展示用）。
func (s *Store) AllEnabledTokenNames() map[string]string {
	out := make(map[string]string)
	rows, err := s.db.Query(`SELECT key, name FROM api_tokens WHERE enabled = 1`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k, n string
		if err := rows.Scan(&k, &n); err == nil && k != "" {
			if n == "" {
				n = k
			}
			out[k] = n
		}
	}
	return out
}

// ===== 管理端用户 =====

// CreateUser 创建管理端用户。
func (s *Store) CreateUser(username, passwordHash string) (*User, error) {
	res, err := s.db.Exec(
		`INSERT INTO users (username, password_hash) VALUES (?, ?)`,
		username, passwordHash,
	)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &User{ID: uint(id), Username: username, PasswordHash: passwordHash, CreatedAt: time.Now().Format(time.RFC3339)}, nil
}

// GetUser 按用户名查询用户。
func (s *Store) GetUser(username string) (*User, error) {
	var u User
	err := s.db.QueryRow(
		`SELECT id, username, password_hash, created_at FROM users WHERE username = ?`,
		username,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// ListUsers 列出所有管理端用户。
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT id, username, password_hash, created_at FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpdateUserPassword 更新用户密码哈希。
func (s *Store) UpdateUserPassword(username, passwordHash string) error {
	_, err := s.db.Exec(`UPDATE users SET password_hash = ? WHERE username = ?`, passwordHash, username)
	return err
}

// CountUsers 返回用户数量。
func (s *Store) CountUsers() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// ===== 渠道优惠时段 =====

// ListDiscounts 列出所有优惠时段。
func (s *Store) ListDiscounts() ([]Discount, error) {
	rows, err := s.db.Query(`SELECT id, upstream, model_pattern, start_time, end_time, discount, note, created_at FROM discounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Discount
	for rows.Next() {
		var d Discount
		if err := rows.Scan(&d.ID, &d.Upstream, &d.ModelPattern, &d.StartTime, &d.EndTime, &d.Discount, &d.Note, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CreateDiscount 创建优惠时段。
func (s *Store) CreateDiscount(d *Discount) error {
	res, err := s.db.Exec(
		`INSERT INTO discounts (upstream, model_pattern, start_time, end_time, discount, note) VALUES (?, ?, ?, ?, ?, ?)`,
		d.Upstream, d.ModelPattern, d.StartTime, d.EndTime, d.Discount, d.Note,
	)
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	d.ID = uint(id)
	d.CreatedAt = time.Now().Format(time.RFC3339)
	return nil
}

// UpdateDiscount 更新优惠时段。
func (s *Store) UpdateDiscount(id uint, d *Discount) error {
	_, err := s.db.Exec(
		`UPDATE discounts SET upstream=?, model_pattern=?, start_time=?, end_time=?, discount=?, note=? WHERE id=?`,
		d.Upstream, d.ModelPattern, d.StartTime, d.EndTime, d.Discount, d.Note, id,
	)
	return err
}

// DeleteDiscount 删除优惠时段。
func (s *Store) DeleteDiscount(id uint) error {
	_, err := s.db.Exec(`DELETE FROM discounts WHERE id = ?`, id)
	return err
}
