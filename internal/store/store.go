// Package store 提供 SQLite 持久化与异步写入。
package store

import (
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，注册 "sqlite" driver 名
)

// Record 是一次转发请求的指标记录。
type Record struct {
	Timestamp             time.Time
	Upstream              string  // 实际转发到的上游名
	Model                 string  // 客户端请求的模型名
	UpstreamModel         string  // 上游真实模型名（计费用；为空则回退用 Model）
	Cost                  float64 // 本次请求费用（元），0 表示未计价
	Endpoint              string  // chat / images / audio / embed / claude / generate
	Status                int     // HTTP 状态码
	DurationMS            int64   // 转发耗时毫秒
	PromptTokens          int64   // 输入 token 数
	CompletionTokens      int64   // 输出 token 数
	Tokens                int64   // 总 token 数 = PromptTokens + CompletionTokens
	APIKey                string  // 下游 API Key 名称（api_tokens.name，用于按 Key 统计）
	ClientAddr            string  // 客户端地址 "IP:port"（r.RemoteAddr 原样），用于区分调用程序
	UserAgent             string  // 客户端 User-Agent（r.UserAgent()，写入时截断 200 字符）
	PromptCacheHitTokens  int64   // 上游前缀缓存命中 token 数（DeepSeek prompt_cache_hit_tokens）
	PromptCacheMissTokens int64   // 上游前缀缓存未命中 token 数（DeepSeek prompt_cache_miss_tokens）
}

// UpstreamRow 是 upstreams 表的行映射。
type UpstreamRow struct {
	ID   uint   `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	// Kind 能力类型：chat（默认）| emb | rerank | tts | asr | image；空 = chat。
	// 健康检查按 Kind 选探测端点，避免把纯 TTS/embedding 等上游用 chat 探测误判 dead。
	Kind          string `json:"kind"`
	BaseURL       string `json:"base_url"`
	APIKey        string `json:"api_key"`
	Tier          string `json:"tier"`
	Priority      int    `json:"priority"`
	Weight        int    `json:"weight"`
	Models        string `json:"models"`
	ModelMapping  string `json:"model_mapping"`
	Enabled       int    `json:"enabled"`        // 1=启用 0=禁用（禁用的不参与转发路由）
	BillingExempt int    `json:"billing_exempt"` // 1=不参与计费（统计费用记 0，路由不受影响）
	// Arrears 欠费标记：1=该上游已因「余额不足」类错误被自动标记。
	// 标记后停止路由与健康检查，等待人工充值后经「测试上游」成功自动清除。
	Arrears         int    `json:"arrears"`
	RequestOverride string `json:"request_override"` // 请求体复写（JSON 字符串）：转发前强制覆盖请求体部分字段，空=不启用
	// Timeout 上游请求超时秒数（连接+非流式整体）；0=跟随全局 retry.upstream_timeout（默认 60）。
	// 慢速兜底上游（如本地一体机）建议单独调大（如 300），避免响应稍慢被全局超时误判失败导致 502。
	Timeout int `json:"timeout"`
	// TimeoutPtr 区分 JSON body 中 timeout 字段"未传"(nil) 与"显式传 0"。
	// UpdateUpstream 用它避免旧客户端未传 timeout 时把已配置的上游超时清零。
	TimeoutPtr *int `json:"-"`
	// KindPtr 区分 JSON body 中 kind 字段"未传"(nil) 与"显式传空串"。
	// UpdateUpstream 用它避免旧客户端未传 kind 时把已配置的能力类型清空回默认 chat。
	KindPtr *string `json:"-"`

	// EnabledPtr 区分 JSON body 中 enabled 字段"未传"(nil) 与"显式传 0/1"。
	// UpdateUpstream 用它避免未传时误禁用上游。
	EnabledPtr *int `json:"-"`
	// BillingExemptPtr 区分 JSON body 中 billing_exempt 字段"未传"(nil) 与"显式传 0/1"。
	BillingExemptPtr *int   `json:"-"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
}

// RoutingRuleRow 是 routing_rules 表的行映射。
type RoutingRuleRow struct {
	ID        uint   `json:"id"`
	Model     string `json:"model"`
	Strategy  string `json:"strategy"`
	Upstreams string `json:"upstreams"` // JSON 数组
	// Vision 是否支持多模态（1=支持，0=不支持）。不支持的规则命中带图请求时，
	// 若配置了 VisionFallback 则把 model 改写为兜底聚合模型名重新路由。
	Vision int64 `json:"vision"`
	// VisionFallback 多模态兜底转发的聚合模型名（如 "flash"），由 model_mapping 映射到上游真实名。
	VisionFallback string `json:"vision_fallback"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

// ConfigRow 是 config 表的行映射（key-value 存储）。
type ConfigRow struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// APIToken 是下游 API Key（api_tokens 表）的行映射。
type APIToken struct {
	ID        uint   `json:"id"`
	Name      string `json:"name"`    // 用途备注，如 "Claude Code" / "Hermes"
	Key       string `json:"key"`     // 下游调用时用的 Bearer token
	Enabled   bool   `json:"enabled"` // 是否启用
	Remark    string `json:"remark"`  // 备注
	CreatedAt string `json:"created_at"`

	// 分组配额（v2）：
	// GroupID=0 表示不归属任何组；AllowedModels 为空=继承组的模型白名单；
	// QuotaOverride 为 JSON {模型:{"5h":..,"week":..,"month":..}}，空={} 表示无 key 级例外。
	GroupID       uint   `json:"group_id"`
	GroupName     string `json:"group_name"`     // 只读：所属组名（LEFT JOIN 带出）
	AllowedModels string `json:"allowed_models"` // key 级模型白名单 JSON
	QuotaOverride string `json:"quota_override"` // key×模型 例外配额 JSON
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

// ModelPrice 是模型单价（model_prices 表）的行映射。单位：元/百万 token。
// model = '*' 表示默认价（所有未单独定价的模型都用它）。
type ModelPrice struct {
	ID         uint    `json:"id"`
	Model      string  `json:"model"`       // 上游真实模型名；'*' = 默认
	PriceInput float64 `json:"price_input"` // 输入（缓存未命中）元/百万token
	PriceCache float64 `json:"price_cache"` // 输入（缓存命中）元/百万token
	PriceOut   float64 `json:"price_out"`   // 输出 元/百万token
	Note       string  `json:"note"`
	CreatedAt  string  `json:"created_at"`
	UpdatedAt  string  `json:"updated_at"`
}

// CostRow 是费用统计的聚合行。
type CostRow struct {
	Name     string  `json:"name"`     // 上游名 / api_key 名 / 模型名
	Cost     float64 `json:"cost"`     // 费用（元）
	Requests int     `json:"requests"` // 请求数
	Tokens   int64   `json:"tokens"`   // 总 token 数
}

// Store 封装 SQLite 连接与表结构。
type Store struct {
	db   *sql.DB
	path string
}

// DBPath 返回数据库文件路径。
func (s *Store) DBPath() string { return s.path }

// BackupDir 返回备份目录（数据库同目录 backups/）。
func (s *Store) BackupDir() string {
	return filepath.Join(filepath.Dir(s.path), "backups")
}

// Open 打开（或创建）SQLite 数据库文件，启用 WAL，建表。
func Open(path string) (*Store, error) {
	// 关键：PRAGMA 通过 DSN 的 _pragma 参数设置，而不是对 db 执行 Exec。
	// busy_timeout / journal_mode / cache_size / mmap_size / synchronous 都是
	// 【每连接】生效；而 database/sql 维护连接池。若用 db.Exec 设置 PRAGMA，
	// 只作用于池中被拿到的第一条连接，随后并发新建的连接仍保留默认值
	// busy_timeout=0，写锁冲突（WAL 下并发写、rebuild/prune/备份等）会立即返回
	// SQLITE_BUSY——正是线上 daily_stats upsert 频繁 “database is locked (5)” 的
	// 根因。modernc.org/sqlite 会把 DSN 中的 _pragma 应用到每一个新建的连接，
	// 且 busy_timeout 排序最先执行。
	dsn := path +
		"?_pragma=busy_timeout(5000)" + // 写锁冲突时等待 5s 而非立即失败
		"&_pragma=journal_mode(WAL)" + // WAL：读不阻塞写，写不阻塞读
		"&_pragma=cache_size(-20000)" + // 页缓存 20MB，热数据常驻内存
		"&_pragma=mmap_size(67108864)" + // 读走内存映射，免磁盘 IO
		"&_pragma=synchronous(NORMAL)" // WAL 下不丢已提交数据，换写入吞吐
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, path: path}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// CreateBackup 用 SQLite 在线快照（VACUUM INTO）备份数据库，再 gzip 压缩。
// VACUUM INTO 是官方推荐的在线一致性备份方式，不会阻塞正在进行的读写。
// 返回备份文件名（backups/<db>.<ts>.gz）。
func (s *Store) CreateBackup() (string, error) {
	dir := s.BackupDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	ts := time.Now().Format("20060102_150405")
	base := filepath.Base(s.path)
	rawPath := filepath.Join(dir, fmt.Sprintf("%s.%s.tmp", base, ts))
	gzPath := filepath.Join(dir, fmt.Sprintf("%s.%s.gz", base, ts))

	// 在线快照到临时文件
	if _, err := s.db.Exec(fmt.Sprintf(`VACUUM INTO '%s'`, rawPath)); err != nil {
		os.Remove(rawPath)
		return "", fmt.Errorf("vacuum: %w", err)
	}
	// gzip 压缩
	if err := gzipFile(rawPath, gzPath); err != nil {
		os.Remove(rawPath)
		return "", fmt.Errorf("gzip: %w", err)
	}
	os.Remove(rawPath)
	return filepath.Base(gzPath), nil
}

// gzipFile 将 src 压缩为 dst（gzip，保留原文件）。
func gzipFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	gz := gzip.NewWriter(out)
	if _, err := io.Copy(gz, in); err != nil {
		gz.Close()
		return err
	}
	return gz.Close()
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
		tokens      INTEGER NOT NULL DEFAULT 0,
		client_addr TEXT    NOT NULL DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS idx_request_log_ts ON request_log(ts);
	CREATE INDEX IF NOT EXISTS idx_request_log_upstream ON request_log(upstream);
	-- 按天聚合（AggregateDay）用 strftime('%Y-%m-%d', ts, '+8 hours') 表达式过滤，
	-- 普通 ts 索引用不上，建表达式索引加速每日统计重算。
	CREATE INDEX IF NOT EXISTS idx_request_log_day ON request_log(strftime('%Y-%m-%d', ts, '+8 hours'));

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
	-- 复合索引 (upstream, ts)：ProbeStats 用 upstream=? AND ts>=? 范围扫描，
	-- 避免遍历该上游全部历史探针（探针表可达百万行）。
	CREATE INDEX IF NOT EXISTS idx_health_probe_upstream_ts ON health_probe_log(upstream, ts);

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
		kind         TEXT    NOT NULL DEFAULT '',    -- 能力类型：chat/emb/rerank/tts/asr/image，空=chat
		created_at   TEXT    NOT NULL DEFAULT (datetime('now')),
		updated_at   TEXT    NOT NULL DEFAULT (datetime('now'))
	);

	CREATE TABLE IF NOT EXISTS routing_rules (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		model           TEXT    NOT NULL UNIQUE,
		strategy        TEXT    NOT NULL DEFAULT '',
		upstreams       TEXT    NOT NULL DEFAULT '[]',
		vision          INTEGER NOT NULL DEFAULT 0,   -- 是否支持多模态（1=支持 0=不支持），默认纯文本模型
		vision_fallback TEXT    NOT NULL DEFAULT '',  -- 多模态兜底聚合模型名（如 "flash"），空=不兜底
		created_at      TEXT    NOT NULL DEFAULT (datetime('now')),
		updated_at      TEXT    NOT NULL DEFAULT (datetime('now'))
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

	CREATE TABLE IF NOT EXISTS model_prices (
		id             INTEGER PRIMARY KEY AUTOINCREMENT,
		model          TEXT    NOT NULL UNIQUE,   -- 上游真实模型名；'*' = 默认价
		price_input    REAL    NOT NULL DEFAULT 1.0,   -- 输入（缓存未命中）元/百万token
		price_output   REAL    NOT NULL DEFAULT 2.0,   -- 输出 元/百万token
		price_cache    REAL    NOT NULL DEFAULT 0.02,  -- 输入（缓存命中）元/百万token
		note           TEXT    NOT NULL DEFAULT '',
		created_at     TEXT    NOT NULL DEFAULT (datetime('now')),
		updated_at     TEXT    NOT NULL DEFAULT (datetime('now'))
	);

	-- 客户端程序分析结果（按 client_addr 唯一，标识 IP:port 对应的调用程序）。
	-- 由分析服务定时/手动触发：聚合 request_log 去重 client_addr，按 User-Agent →
	-- 端口查进程识别，结果经应用层 API upsert 到此表，前端"客户端分析"页展示。
	CREATE TABLE IF NOT EXISTS client_profiles (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		client_addr TEXT    NOT NULL UNIQUE,    -- IP:port
		program     TEXT    NOT NULL DEFAULT '',   -- 识别出的程序名
		confidence  REAL    NOT NULL DEFAULT 0,    -- 置信度 0-1
		evidence    TEXT    NOT NULL DEFAULT '',   -- 分析依据（UA、端口、行为特征等）
		updated_at      TEXT    NOT NULL DEFAULT (datetime('now'))
	);

	CREATE TABLE IF NOT EXISTS daily_stats (
		date              TEXT PRIMARY KEY,
		requests          INTEGER NOT NULL DEFAULT 0,
		successes         INTEGER NOT NULL DEFAULT 0,
		tokens            INTEGER NOT NULL DEFAULT 0,
		prompt_tokens     INTEGER NOT NULL DEFAULT 0,
		completion_tokens INTEGER NOT NULL DEFAULT 0,
		cache_hit_tokens  INTEGER NOT NULL DEFAULT 0,
		cache_miss_tokens INTEGER NOT NULL DEFAULT 0,
		cost              REAL    NOT NULL DEFAULT 0,
		sum_duration_ms   INTEGER NOT NULL DEFAULT 0,
		by_upstream       TEXT    NOT NULL DEFAULT '{}',
		by_api_key        TEXT    NOT NULL DEFAULT '{}',
		by_model          TEXT    NOT NULL DEFAULT '{}',
		computed_at       TEXT    NOT NULL DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS idx_daily_stats_date ON daily_stats(date);
	`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	// 迁移：新增 prompt_tokens / completion_tokens 列（已存在的忽略错误）
	for _, col := range []string{"prompt_tokens", "completion_tokens"} {
		s.db.Exec("ALTER TABLE request_log ADD COLUMN " + col + " INTEGER NOT NULL DEFAULT 0")
	}
	// 迁移：新增 prefix 缓存命中统计列（DeepSeek prompt_cache_hit/miss_tokens）
	for _, col := range []string{"prompt_cache_hit_tokens", "prompt_cache_miss_tokens"} {
		s.db.Exec("ALTER TABLE request_log ADD COLUMN " + col + " INTEGER NOT NULL DEFAULT 0")
	}
	// 迁移：request_log 加 api_key 列（按下游 Key 统计）
	s.db.Exec("ALTER TABLE request_log ADD COLUMN api_key TEXT NOT NULL DEFAULT ''")
	s.db.Exec("CREATE INDEX IF NOT EXISTS idx_request_log_apikey ON request_log(api_key)")
	// 迁移：request_log 加上游真实模型名列（计费按上游真实名查价）
	s.db.Exec("ALTER TABLE request_log ADD COLUMN upstream_model TEXT NOT NULL DEFAULT ''")
	// 迁移：request_log 加 cost 列（本次请求费用，元）
	s.db.Exec("ALTER TABLE request_log ADD COLUMN cost REAL NOT NULL DEFAULT 0")
	// 迁移：request_log 加 client_addr 列（客户端地址 "IP:port"，按调用程序分析）
	s.db.Exec("ALTER TABLE request_log ADD COLUMN client_addr TEXT NOT NULL DEFAULT ''")
	// 迁移：request_log 加 user_agent 列（客户端 User-Agent，程序识别最强信号）。
	// 用 PRAGMA table_info 判断列是否存在，保证旧库（无此列）与新建库都幂等可启动。
	ensureColumn(s.db, "request_log", "user_agent", "user_agent TEXT NOT NULL DEFAULT ''")
	// 迁移：upstreams 加 enabled 列（禁用/启用）
	ensureColumn(s.db, "upstreams", "enabled", "enabled INTEGER NOT NULL DEFAULT 1")
	// 迁移：upstreams 加 billing_exempt 列（不参与计费：统计模块费用记 0，路由不受影响）
	ensureColumn(s.db, "upstreams", "billing_exempt", "billing_exempt INTEGER NOT NULL DEFAULT 0")
	// 迁移：upstreams 加 request_override 列（请求体复写：转发前强制覆盖请求体部分字段）
	ensureColumn(s.db, "upstreams", "request_override", "request_override TEXT NOT NULL DEFAULT ''")
	// 迁移：upstreams 加 arrears 列（欠费标记：停路由+停健康检查，测试通过后自动清除）
	ensureColumn(s.db, "upstreams", "arrears", "arrears INTEGER NOT NULL DEFAULT 0")
	// 迁移：upstreams 加 timeout 列（上游请求超时秒数；0=跟随全局 retry.upstream_timeout）
	ensureColumn(s.db, "upstreams", "timeout", "timeout INTEGER NOT NULL DEFAULT 0")
	// 迁移：upstreams 加 kind 列（能力类型 chat|emb|rerank|tts|asr|image，健康检查按此选探测端点；空=chat）
	ensureColumn(s.db, "upstreams", "kind", "kind TEXT NOT NULL DEFAULT ''")
	// 迁移：routing_rules 加 vision / vision_fallback 列（多模态兜底，老库自动补列）
	ensureColumn(s.db, "routing_rules", "vision", "vision INTEGER NOT NULL DEFAULT 0")
	ensureColumn(s.db, "routing_rules", "vision_fallback", "vision_fallback TEXT NOT NULL DEFAULT ''")

	// 分组管理（配额按模型独立池、组内每人默认，key 可覆盖）
	if _, err := s.db.Exec(`
	CREATE TABLE IF NOT EXISTS groups (
		id             INTEGER PRIMARY KEY AUTOINCREMENT,
		name           TEXT    NOT NULL UNIQUE,
		allowed_models TEXT    NOT NULL DEFAULT '[]',  -- JSON 数组；空/[] = 不限
		remark         TEXT    NOT NULL DEFAULT '',
		created_at     TEXT    NOT NULL DEFAULT (datetime('now'))
	);
	CREATE TABLE IF NOT EXISTS group_model_quota (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		group_id     INTEGER NOT NULL,
		model        TEXT    NOT NULL DEFAULT '',      -- 模型名；'*' = 全模型兜底
		token_5h     INTEGER NOT NULL DEFAULT 0,
		token_week   INTEGER NOT NULL DEFAULT 0,
		token_month  INTEGER NOT NULL DEFAULT 0,
		UNIQUE(group_id, model)
	);
	`); err != nil {
		return err
	}
	// 迁移：api_tokens 加分组与覆盖策略列（group_id=0 表示不归属组）
	ensureColumn(s.db, "api_tokens", "group_id", "group_id INTEGER NOT NULL DEFAULT 0")
	ensureColumn(s.db, "api_tokens", "allowed_models", "allowed_models TEXT NOT NULL DEFAULT ''")
	ensureColumn(s.db, "api_tokens", "quota_override", "quota_override TEXT NOT NULL DEFAULT '{}'")
	return nil
}

// ensureColumn 幂等添加列：先查 PRAGMA table_info 判断列是否已存在，已存在则跳过。
// SQLite 的 ALTER TABLE ADD COLUMN 对已存在列会报 duplicate column name，
// 不能只依赖忽略错误——旧库与新库结构不同，显式判断最稳妥。
// table 参数只传内部常量（如 "request_log"），不接用户输入。
func ensureColumn(db *sql.DB, table, column, ddl string) {
	// pragma_table_info 是表值函数形式，支持绑定参数（PRAGMA table_info(?) 不支持）
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		slog.Warn("ensureColumn: table_info failed", "table", table, "error", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			slog.Warn("ensureColumn: scan failed", "table", table, "error", err)
			return
		}
		if name == column {
			return // 列已存在，跳过
		}
	}
	if _, err := db.Exec("ALTER TABLE " + table + " ADD COLUMN " + ddl); err != nil {
		slog.Warn("ensureColumn: alter failed", "table", table, "column", column, "error", err)
	}
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
		`INSERT INTO request_log (ts, upstream, model, endpoint, status, duration_ms, tokens, prompt_tokens, completion_tokens, api_key, client_addr, user_agent, prompt_cache_hit_tokens, prompt_cache_miss_tokens)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Timestamp.UTC().Format(time.RFC3339), rec.Upstream, rec.Model, rec.Endpoint,
		rec.Status, rec.DurationMS, rec.Tokens, rec.PromptTokens, rec.CompletionTokens, rec.APIKey,
		rec.ClientAddr, rec.UserAgent, rec.PromptCacheHitTokens, rec.PromptCacheMissTokens,
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

// ProbeStatsAll 一次性返回所有上游的探针成功/失败计数（GROUP BY upstream）。
// 替代 MetricsUpstreams 里对每个上游串行调用 ProbeStats 的 N 次查询，
// 显著降低 30d 接口延迟（探针表可达百万行）。
func (s *Store) ProbeStatsAll(since string) map[string][2]int64 {
	q := `SELECT upstream, COALESCE(SUM(CASE WHEN ok=1 THEN 1 ELSE 0 END),0),
	               COALESCE(SUM(CASE WHEN ok=0 THEN 1 ELSE 0 END),0)
	       FROM health_probe_log`
	var args []any
	if since != "" {
		q += ` WHERE ts >= ?`
		args = append(args, since)
	}
	q += ` GROUP BY upstream`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[string][2]int64{}
	for rows.Next() {
		var u string
		var okc, failc int64
		if err := rows.Scan(&u, &okc, &failc); err != nil {
			continue
		}
		out[u] = [2]int64{okc, failc}
	}
	return out
}

// ProbeRetainDays 探针记录保留天数：仅保留最近 N 天健康检查探针，
// 控制 health_probe_log 规模（每天约数万条，无清理会无限膨胀拖慢读写）。
const ProbeRetainDays = 7

// PruneHealthProbeLog 删除超过 retainDays 的探针记录（ts 为 UTC RFC3339 文本，
// 走 idx_health_probe_ts 索引范围删除）。返回删除行数。
// 由 dailyStatsTicker 在启动时 + 每日调用维护。
func (s *Store) PruneHealthProbeLog(retainDays int) (int64, error) {
	if retainDays <= 0 {
		return 0, nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -retainDays).Format(time.RFC3339)
	res, err := s.db.Exec(`DELETE FROM health_probe_log WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RequestLogRetainDays 请求明细保留天数：仅保留最近 N 天原始请求记录。
// 历史统计已由 daily_stats 按天预聚合兜底，原始明细仅需保留近期
// （用于单条追溯 + today 实时聚合），更早的明细每日自动删除控制表规模。
const RequestLogRetainDays = 30

// PruneRequestLog 删除超过 retainDays 的请求明细（ts 为 UTC RFC3339，
// 走 idx_request_log_ts 索引范围删除）。返回删除行数。
// 由 dailyStatsTicker 每日维护；删除前自动备份（backupOnce 每日轮转）兜底。
func (s *Store) PruneRequestLog(retainDays int) (int64, error) {
	if retainDays <= 0 {
		return 0, nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -retainDays).Format(time.RFC3339)
	res, err := s.db.Exec(`DELETE FROM request_log WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// APIKeyRow 是按下游 Key 聚合的统计行。
type APIKeyRow struct {
	Name         string
	Requests     int64
	Successes    int64
	AvgLatencyMS float64
	TotalTokens  int64
	CacheHit     int64
	CacheMiss    int64
}

// MetricsByAPIKey 返回按下游 API Key 聚合的统计（用于"哪些程序用得多"）。
// MetricsByAPIKey 按下游 API Key 聚合请求量指标（按 token 降序）。
func (s *Store) MetricsByAPIKey(since string) []APIKeyRow {
	q := `SELECT COALESCE(NULLIF(api_key, ''), (SELECT name FROM api_tokens WHERE enabled=1 ORDER BY id LIMIT 1), '(未标识)') as name,
	              COUNT(*) as requests,
	              COALESCE(SUM(CASE WHEN status < 400 THEN 1 ELSE 0 END), 0) as successes,
	              COALESCE(AVG(duration_ms), 0) as avg_ms,
	              COALESCE(SUM(tokens), 0) as tokens,
	              COALESCE(SUM(prompt_cache_hit_tokens), 0) as cache_hit,
	              COALESCE(SUM(prompt_cache_miss_tokens), 0) as cache_miss
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
		if err := rows.Scan(&r.Name, &r.Requests, &r.Successes, &r.AvgLatencyMS, &r.TotalTokens, &r.CacheHit, &r.CacheMiss); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out
}

// PriceFor 返回指定（上游真实）模型名的单价。没单独定价时回退到默认价（'*'）。
// 返回四个值：输入价、缓存命中输入价、输出价、是否找到。
func (s *Store) PriceFor(model string) (input, cache, out float64, ok bool) {
	if model != "" {
		row := s.db.QueryRow(`SELECT price_input, price_cache, price_output FROM model_prices WHERE model = ?`, model)
		if err := row.Scan(&input, &cache, &out); err == nil {
			return input, cache, out, true
		}
	}
	row := s.db.QueryRow(`SELECT price_input, price_cache, price_output FROM model_prices WHERE model = '*'`)
	if err := row.Scan(&input, &cache, &out); err == nil {
		return input, cache, out, true
	}
	return 0, 0, 0, false
}

// ListPrices 返回全部模型单价（默认价 '*' 排第一）。
func (s *Store) ListPrices() []ModelPrice {
	rows, err := s.db.Query(`SELECT id, model, price_input, price_cache, price_output, note, created_at, updated_at FROM model_prices ORDER BY (model='*') DESC, model ASC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []ModelPrice
	for rows.Next() {
		var r ModelPrice
		if err := rows.Scan(&r.ID, &r.Model, &r.PriceInput, &r.PriceCache, &r.PriceOut, &r.Note, &r.CreatedAt, &r.UpdatedAt); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out
}

// RecalcCost 重新计算历史请求费用：对缓存字段全 0（上游没返回缓存统计）
// 且有输入 token 的请求，按「未命中价全额」口径重算 cost（与 calcCost 修复后
// 的逻辑一致：无缓存统计时输入按未命中价计费）。
// 返回更新条数。事务内执行，失败自动回滚。
func (s *Store) RecalcCost() (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT id, model, upstream_model, prompt_tokens, completion_tokens, cost
		FROM request_log
		WHERE prompt_cache_hit_tokens <= 0 AND prompt_cache_miss_tokens <= 0 AND prompt_tokens > 0`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type row struct {
		id             int64
		model          string
		upstreamModel  string
		promptTokens   int64
		completionToks int64
		oldCost        float64
	}
	var targets []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.model, &r.upstreamModel, &r.promptTokens, &r.completionToks, &r.oldCost); err != nil {
			continue
		}
		targets = append(targets, r)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	const perMillion = 1e6
	updated := int64(0)
	for _, r := range targets {
		// 与 proxy.calcCost 口径一致：优先上游真实模型名，其次客户端模型名，最后默认价
		input, _, out, ok := s.PriceFor(r.upstreamModel)
		if !ok {
			input, _, out, ok = s.PriceFor(r.model)
		}
		if !ok || (input <= 0 && out <= 0) {
			continue // 无价格表，跳过（保持原值）
		}
		newCost := float64(r.promptTokens)/perMillion*input + float64(r.completionToks)/perMillion*out
		if _, err := tx.Exec(`UPDATE request_log SET cost = ? WHERE id = ?`, newCost, r.id); err != nil {
			return 0, err
		}
		updated++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return updated, nil
}

// UpsertPrice 新增或更新模型单价（按 model 唯一）。
func (s *Store) UpsertPrice(p ModelPrice) error {
	_, err := s.db.Exec(`INSERT INTO model_prices (model, price_input, price_cache, price_output, note)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(model) DO UPDATE SET
			price_input = excluded.price_input,
			price_cache = excluded.price_cache,
			price_output = excluded.price_output,
			note = excluded.note,
			updated_at = datetime('now')`,
		p.Model, p.PriceInput, p.PriceCache, p.PriceOut, p.Note)
	return err
}

// DeletePrice 删除模型单价。
func (s *Store) DeletePrice(model string) error {
	_, err := s.db.Exec(`DELETE FROM model_prices WHERE model = ?`, model)
	return err
}

// EnsureDefaultPrice 确保默认价存在（'*'）。默认按 deepseek-v4-flash 定价：
// 输入缓存命中 0.02 元/百万token，输入缓存未命中 1 元/百万token，输出 2 元/百万token。
func (s *Store) EnsureDefaultPrice() {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO model_prices (model, price_input, price_cache, price_output, note)
		VALUES ('*', 1.0, 0.02, 2.0, '默认价：按 deepseek-v4-flash 定价')`)
	if err != nil {
		slog.Error("ensure default price", "error", err)
	}
}

// TotalCost 返回指定时间段内总费用（元）。
// billing_exempt=1 的上游（标记为不参与计费）在统计中费用记 0，但请求日志照常记录。
func (s *Store) TotalCost(since, until string) (float64, int) {
	q := `SELECT COALESCE(SUM(CASE WHEN u.billing_exempt = 1 THEN 0 ELSE rl.cost END), 0), COUNT(*)
	       FROM request_log rl LEFT JOIN upstreams u ON u.name = rl.upstream WHERE rl.cost > 0`
	var args []any
	if since != "" {
		q += ` AND rl.ts >= ?`
		args = append(args, since)
	}
	if until != "" {
		q += ` AND rl.ts <= ?`
		args = append(args, until)
	}
	var cost float64
	var cnt int
	if err := s.db.QueryRow(q, args...).Scan(&cost, &cnt); err != nil {
		return 0, 0
	}
	return cost, cnt
}

// CostByUpstream 按上游聚合费用。
func (s *Store) CostByUpstream(since, until string) []CostRow {
	return s.costGroupBy(`upstream`, since, until)
}

// CostByAPIKey 按下游 API Key 聚合费用（含总 token）。
func (s *Store) CostByAPIKey(since, until string) []CostRow {
	return s.costGroupBy(`api_key`, since, until)
}

// CostByModel 按上游真实模型名聚合费用（费用饼图的二级拆分）。
func (s *Store) CostByModel(since, until string) []CostRow {
	return s.costGroupBy(`upstream_model`, since, until)
}

// costGroupBy 通用费用聚合。
// billing_exempt=1 的上游（不参与计费）在统计中费用记 0，但请求日志照常记录。
func (s *Store) costGroupBy(col, since, until string) []CostRow {
	q := `SELECT COALESCE(NULLIF(rl.` + col + `, ''), '(未知)'),
	              COALESCE(SUM(CASE WHEN u.billing_exempt = 1 THEN 0 ELSE rl.cost END), 0),
	              COUNT(*), COALESCE(SUM(rl.tokens), 0)
	       FROM request_log rl LEFT JOIN upstreams u ON u.name = rl.upstream WHERE rl.cost > 0`
	var args []any
	if since != "" {
		q += ` AND rl.ts >= ?`
		args = append(args, since)
	}
	if until != "" {
		q += ` AND rl.ts <= ?`
		args = append(args, until)
	}
	q += ` GROUP BY rl.` + col + ` ORDER BY SUM(CASE WHEN u.billing_exempt = 1 THEN 0 ELSE rl.cost END) DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []CostRow
	for rows.Next() {
		var r CostRow
		if err := rows.Scan(&r.Name, &r.Cost, &r.Requests, &r.Tokens); err != nil {
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
		`INSERT INTO request_log (ts, upstream, model, upstream_model, cost, endpoint, status, duration_ms, tokens, prompt_tokens, completion_tokens, api_key, client_addr, user_agent, prompt_cache_hit_tokens, prompt_cache_miss_tokens)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, rec := range recs {
		if _, err := stmt.Exec(
			rec.Timestamp.UTC().Format(time.RFC3339), rec.Upstream, rec.Model, rec.UpstreamModel, rec.Cost,
			rec.Endpoint,
			rec.Status, rec.DurationMS, rec.Tokens, rec.PromptTokens, rec.CompletionTokens, rec.APIKey,
			rec.ClientAddr, rec.UserAgent, rec.PromptCacheHitTokens, rec.PromptCacheMissTokens,
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

	usageHook func(Record) // 实时用量钩子（配额内存计数用）；nil 不调用
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
// user_agent 在此统一截断 200 字符，避免超长 UA（如完整浏览器 UA 字符串）撑爆 request_log。
func (r *Recorder) Record(rec Record) {
	if len(rec.UserAgent) > 200 {
		rec.UserAgent = rec.UserAgent[:200]
	}
	// 实时用量钩子（配额内存计数）：在投递落库前同步调用，保证配额判定即时生效
	// （异步批量落库存在 flush 窗口，不能依赖 request_log 做实时判定）。
	if r.usageHook != nil {
		r.usageHook(rec)
	}
	select {
	case r.ch <- rec:
	default:
		r.dropOnce.Do(func() {
			r.log.Warn("store: recorder channel full, dropping records")
		})
	}
}

// SetUsageHook 注入实时用量回调（main 接线到 quota.Service.AddUsage）。
func (r *Recorder) SetUsageHook(f func(Record)) {
	if r != nil {
		r.usageHook = f
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
	rows, err := s.db.Query(`SELECT id, name, type, kind, base_url, api_key, tier, priority, weight, models, model_mapping, enabled, billing_exempt, request_override, timeout, arrears, created_at, updated_at FROM upstreams ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []UpstreamRow
	for rows.Next() {
		var u UpstreamRow
		if err := rows.Scan(&u.ID, &u.Name, &u.Type, &u.Kind, &u.BaseURL, &u.APIKey, &u.Tier, &u.Priority, &u.Weight, &u.Models, &u.ModelMapping, &u.Enabled, &u.BillingExempt, &u.RequestOverride, &u.Timeout, &u.Arrears, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// GetUpstream 按名称查询上游。
func (s *Store) GetUpstream(name string) (*UpstreamRow, error) {
	var u UpstreamRow
	err := s.db.QueryRow(`SELECT id, name, type, kind, base_url, api_key, tier, priority, weight, models, model_mapping, enabled, billing_exempt, request_override, timeout, arrears, created_at, updated_at FROM upstreams WHERE name = ?`, name).
		Scan(&u.ID, &u.Name, &u.Type, &u.Kind, &u.BaseURL, &u.APIKey, &u.Tier, &u.Priority, &u.Weight, &u.Models, &u.ModelMapping, &u.Enabled, &u.BillingExempt, &u.RequestOverride, &u.Timeout, &u.Arrears, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// CreateUpstream 创建上游。enabled 未传（nil）时默认启用（1）。
// billing_exempt 未传（nil）时默认不豁免（0）。
func (s *Store) CreateUpstream(u *UpstreamRow) error {
	enabled := 1
	if u.EnabledPtr != nil {
		enabled = *u.EnabledPtr
	}
	billingExempt := 0
	if u.BillingExemptPtr != nil {
		billingExempt = *u.BillingExemptPtr
	}
	_, err := s.db.Exec(
		`INSERT INTO upstreams (name, type, kind, base_url, api_key, tier, priority, weight, models, model_mapping, enabled, billing_exempt, request_override, timeout) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.Name, u.Type, u.Kind, u.BaseURL, u.APIKey, u.Tier, u.Priority, u.Weight, u.Models, u.ModelMapping, enabled, billingExempt, u.RequestOverride, u.Timeout,
	)
	return err
}

// UpdateUpstream 更新上游（按名称匹配）。
// enabled 用指针区分"未传"（nil=保持原值）与"显式传 0/1"，
// 避免前端编辑表单不传 enabled 时误把上游禁用。
// billing_exempt 同样用指针区分，未传保持原值。
func (s *Store) UpdateUpstream(name string, u *UpstreamRow) error {
	var setExpr string
	var args []any
	args = append(args, u.Name, u.Type, u.Kind, u.BaseURL, u.APIKey, u.Tier, u.Priority, u.Weight, u.Models, u.ModelMapping, u.RequestOverride)
	if u.EnabledPtr != nil {
		setExpr += ", enabled=?"
		args = append(args, *u.EnabledPtr)
	}
	if u.BillingExemptPtr != nil {
		setExpr += ", billing_exempt=?"
		args = append(args, *u.BillingExemptPtr)
	}
	if u.TimeoutPtr != nil {
		setExpr += ", timeout=?"
		args = append(args, *u.TimeoutPtr)
	}
	if u.KindPtr != nil {
		setExpr += ", kind=?"
		args = append(args, *u.KindPtr)
	}
	args = append(args, name)
	_, err := s.db.Exec(
		`UPDATE upstreams SET name=?, type=?, kind=?, base_url=?, api_key=?, tier=?, priority=?, weight=?, models=?, model_mapping=?, request_override=?`+setExpr+`, updated_at=datetime('now') WHERE name=?`,
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

// SetUpstreamArrears 写入欠费标记（1=标记停路由停健康检查，0=恢复）。
func (s *Store) SetUpstreamArrears(name string, arrears int) error {
	if arrears != 0 {
		arrears = 1
	}
	_, err := s.db.Exec(`UPDATE upstreams SET arrears=?, updated_at=datetime('now') WHERE name=?`, arrears, name)
	return err
}

// IsArrears 返回上游是否处于欠费标记中；查询失败视为 false（不阻断转发）。
func (s *Store) IsArrears(name string) bool {
	var n int
	err := s.db.QueryRow(`SELECT arrears FROM upstreams WHERE name = ?`, name).Scan(&n)
	return err == nil && n == 1
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
	// 级联删除请求日志中涉及该上游的记录。
	// upstream 列可能逗号分隔多个上游（如 "a,b"），用四种模式精确覆盖：
	// 精确匹配 / 前缀 / 中间 / 后缀。
	if _, err := tx.Exec(
		`DELETE FROM request_log WHERE upstream = ? OR upstream LIKE ? OR upstream LIKE ? OR upstream LIKE ?`,
		name, name+",%", "%,"+name+",%", "%,"+name,
	); err != nil {
		return err
	}
	// 清理所有规则 JSON 里的引用
	rows, err := tx.Query(`SELECT id, upstreams FROM routing_rules`)
	if err != nil {
		return err
	}
	type ruleRow struct {
		id        int64
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
	if err := tx.Commit(); err != nil {
		return err
	}
	// 事务提交后重算每日统计：基于已清干净 request_log 重新聚合，
	// 被删上游的统计数据不再残留（全局汇总与按上游之和保持一致）。
	if err := s.RebuildDailyStats(); err != nil {
		slog.Error("rebuild daily stats after upstream delete", "upstream", name, "error", err)
	}
	return nil
}

// PurgeOrphanUpstreamLogs 一次性清理：删除请求日志中涉及"已在上游管理中删除"的
// 上游的记录，并重算每日统计，使统计不再残留僵尸上游。返回删除的请求日志行数。
// 用于上线"删除上游级联清理"功能后，清理历史遗留的脏数据。
// 与 DeleteUpstream 的单条级联删除不同，此方法一次性处理所有僵尸上游，且仅重算一次。
func (s *Store) PurgeOrphanUpstreamLogs() (int64, error) {
	// 现存上游名集合
	liveRows, err := s.db.Query(`SELECT name FROM upstreams`)
	if err != nil {
		return 0, err
	}
	live := map[string]bool{}
	for liveRows.Next() {
		var n string
		if err := liveRows.Scan(&n); err != nil {
			liveRows.Close()
			return 0, err
		}
		live[n] = true
	}
	liveRows.Close()

	// 收集 request_log 中出现过的所有上游 token，找出僵尸（不在 upstreams 表）
	distinct, err := s.db.Query(`SELECT DISTINCT upstream FROM request_log WHERE upstream IS NOT NULL AND upstream != ''`)
	if err != nil {
		return 0, err
	}
	zombies := map[string]bool{}
	for distinct.Next() {
		var u string
		if err := distinct.Scan(&u); err != nil {
			distinct.Close()
			return 0, err
		}
		for _, tok := range strings.Split(u, ",") {
			tok = strings.TrimSpace(tok)
			if tok != "" && !live[tok] {
				zombies[tok] = true
			}
		}
	}
	distinct.Close()

	if len(zombies) == 0 {
		return 0, nil
	}

	var totalDeleted int64
	for name := range zombies {
		res, err := s.db.Exec(
			`DELETE FROM request_log WHERE upstream = ? OR upstream LIKE ? OR upstream LIKE ? OR upstream LIKE ?`,
			name, name+",%", "%,"+name+",%", "%,"+name,
		)
		if err != nil {
			return totalDeleted, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			totalDeleted += n
		}
	}
	// 基于已清干净的 request_log 重算统计（只重算一次）
	if err := s.RebuildDailyStats(); err != nil {
		slog.Error("rebuild daily stats after purge", "error", err)
	}
	return totalDeleted, nil
}

// mustJSON 将 []string 编码为 JSON 字符串（忽略错误，调用方保证可序列化）。
func mustJSON(v []string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ===== Routing Rules CRUD =====

// ListRoutingRules 返回所有路由规则。
func (s *Store) ListRoutingRules() ([]RoutingRuleRow, error) {
	rows, err := s.db.Query(`SELECT id, model, strategy, upstreams, vision, vision_fallback, created_at, updated_at FROM routing_rules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RoutingRuleRow
	for rows.Next() {
		var r RoutingRuleRow
		if err := rows.Scan(&r.ID, &r.Model, &r.Strategy, &r.Upstreams, &r.Vision, &r.VisionFallback, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRoutingRule 按 model 查询。
func (s *Store) GetRoutingRule(model string) (*RoutingRuleRow, error) {
	var r RoutingRuleRow
	err := s.db.QueryRow(`SELECT id, model, strategy, upstreams, vision, vision_fallback, created_at, updated_at FROM routing_rules WHERE model = ?`, model).
		Scan(&r.ID, &r.Model, &r.Strategy, &r.Upstreams, &r.Vision, &r.VisionFallback, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// CreateRoutingRule 创建路由规则。
func (s *Store) CreateRoutingRule(r *RoutingRuleRow) error {
	_, err := s.db.Exec(
		`INSERT INTO routing_rules (model, strategy, upstreams, vision, vision_fallback) VALUES (?, ?, ?, ?, ?)`,
		r.Model, r.Strategy, r.Upstreams, r.Vision, r.VisionFallback,
	)
	return err
}

// UpdateRoutingRule 更新路由规则（按 model 匹配）。
func (s *Store) UpdateRoutingRule(model string, r *RoutingRuleRow) error {
	_, err := s.db.Exec(
		`UPDATE routing_rules SET model=?, strategy=?, upstreams=?, vision=?, vision_fallback=?, updated_at=datetime('now') WHERE model=?`,
		r.Model, r.Strategy, r.Upstreams, r.Vision, r.VisionFallback, model,
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
	ID          uint   `json:"id"`
	Model       string `json:"model"`       // 模型匹配 pattern（支持 * 通配）
	Recommended string `json:"recommended"` // 推荐思考等级（客户端未传时自动补）
	Forced      string `json:"forced"`      // 强制思考等级（覆盖客户端传的）
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
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
		"retry.fast_fail_minutes":       "5",
		"retry.fast_fail_probe_minutes": "5",
		"retry.upstream_timeout":        "60",
		// 视频透传（默认关）
		"proxy.video_pass_through": "false",
		// reasoning_content 回传缓存（DeepSeek thinking 模式 tool-calling 兼容，默认开）
		"proxy.cache_reasoning_content": "true",
		// per-key 冷却（商汤默认 1 秒）
		"proxy.cooldown_seconds":   "1",
		"proxy.cooldown_upstreams": "[\"商汤\"]",
		// 最佳思考等级（默认关）
		"proxy.auto_best_effort":  "false",
		"proxy.force_best_effort": "false",
		// 客户端程序分析（默认关；开启后按间隔定时按 UA/端口查进程识别 client_addr 对应程序）
		"proxy.client_analysis":          "false",
		"proxy.client_analysis_interval": "600",
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

// ListAPITokens 列出所有下游 API Key（含分组字段与组名）。
func (s *Store) ListAPITokens() ([]APIToken, error) {
	rows, err := s.db.Query(`SELECT t.id, t.name, t.key, t.enabled, t.remark, t.created_at,
		t.group_id, COALESCE(g.name, ''), t.allowed_models, t.quota_override
		FROM api_tokens t LEFT JOIN groups g ON g.id = t.group_id
		ORDER BY t.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []APIToken
	for rows.Next() {
		var t APIToken
		var enabled int
		if err := rows.Scan(&t.ID, &t.Name, &t.Key, &enabled, &t.Remark, &t.CreatedAt,
			&t.GroupID, &t.GroupName, &t.AllowedModels, &t.QuotaOverride); err != nil {
			return nil, err
		}
		t.Enabled = enabled != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetAPITokenByKey 按 key 字符串取整行（含分组/覆盖策略），key 不存在返回 (nil, nil)。
func (s *Store) GetAPITokenByKey(key string) (*APIToken, error) {
	var t APIToken
	var enabled int
	err := s.db.QueryRow(`SELECT t.id, t.name, t.key, t.enabled, t.remark, t.created_at,
		t.group_id, COALESCE(g.name, ''), t.allowed_models, t.quota_override
		FROM api_tokens t LEFT JOIN groups g ON g.id = t.group_id
		WHERE t.key = ?`, key).Scan(
		&t.ID, &t.Name, &t.Key, &enabled, &t.Remark, &t.CreatedAt,
		&t.GroupID, &t.GroupName, &t.AllowedModels, &t.QuotaOverride)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.Enabled = enabled != 0
	return &t, nil
}

// UpdateAPITokenPolicy 更新下游 key 的分组归属与覆盖策略。
// allowedModels 与 quotaOverride 传空字符串表示不修改（保留原值）。
func (s *Store) UpdateAPITokenPolicy(id uint, groupID uint, allowedModels, quotaOverride string) error {
	q := `UPDATE api_tokens SET `
	var args []interface{}
	add := func(expr string, v interface{}) {
		if len(args) > 0 {
			q += ", "
		}
		q += expr
		args = append(args, v)
	}
	add("group_id = ?", groupID)
	if strings.TrimSpace(allowedModels) != "" {
		add("allowed_models = ?", strings.TrimSpace(allowedModels))
	}
	if strings.TrimSpace(quotaOverride) != "" {
		add("quota_override = ?", strings.TrimSpace(quotaOverride))
	}
	q += " WHERE id = ?"
	args = append(args, id)
	_, err := s.db.Exec(q, args...)
	return err
}

// WindowTokenSum 返回某 key（按 api_tokens.name 快照）在 since 之后、指定模型的 token 总量。
// model 为空串表示不限模型。用于配额窗口检查。
func (s *Store) WindowTokenSum(name, model string, since time.Time) (int64, error) {
	q := `SELECT COALESCE(SUM(tokens), 0) FROM request_log WHERE api_key = ? AND status < 400 AND ts >= ?`
	args := []interface{}{name, since.Format(time.RFC3339)}
	if model != "" {
		q += ` AND model = ?`
		args = append(args, model)
	}
	var n int64
	if err := s.db.QueryRow(q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// WindowTokenEntries 返回某 key 在 since 之后、指定模型的 (时间戳, tokens) 明细，按时序升序。
// 供配额懒加载重建近 5h 滑动窗口（不折叠，保证精确滑出）。
func (s *Store) WindowTokenEntries(name, model string, since time.Time) ([]quotaEntry, error) {
	q := `SELECT ts, tokens FROM request_log WHERE api_key = ? AND status < 400 AND ts >= ?`
	args := []interface{}{name, since.Format(time.RFC3339)}
	if model != "" {
		q += ` AND model = ?`
		args = append(args, model)
	}
	q += ` ORDER BY ts ASC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []quotaEntry
	for rows.Next() {
		var ts string
		var tk int64
		if err := rows.Scan(&ts, &tk); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339, ts); err == nil && tk > 0 {
			out = append(out, quotaEntry{At: t, Tokens: tk})
		}
	}
	return out, rows.Err()
}

// quotaEntry 是 (时间戳, tokens) 明文明细（request_log 行），供配额包重建滑动窗口。
type quotaEntry struct {
	At     time.Time
	Tokens int64
}

// GroupWindowTokenSum 返回某组内所有 key 在 since 之后、指定模型的 token 总量（组视图统计用）。
func (s *Store) GroupWindowTokenSum(groupID uint, model string, since time.Time) (int64, error) {
	q := `SELECT COALESCE(SUM(r.tokens), 0) FROM request_log r
		JOIN api_tokens t ON r.api_key = t.name
		WHERE t.group_id = ? AND r.status < 400 AND r.ts >= ?`
	args := []interface{}{groupID, since.Format(time.RFC3339)}
	if model != "" {
		q += ` AND r.model = ?`
		args = append(args, model)
	}
	var n int64
	if err := s.db.QueryRow(q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
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

// UpdateAPITokenName 修改下游 API Key 的名称（按 id）。
func (s *Store) UpdateAPITokenName(id uint, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("name is required")
	}
	_, err := s.db.Exec(`UPDATE api_tokens SET name = ? WHERE id = ?`, strings.TrimSpace(name), id)
	return err
}

// RenameLogAPIKey 同步 request_log 中历史记录的名称快照（旧名 → 新名）。
// 请求日志写入时存的是当时 api_tokens.name 的快照，改名后需同步以保持显示一致。
func (s *Store) RenameLogAPIKey(oldName, newName string) error {
	if oldName == "" || newName == "" {
		return nil
	}
	_, err := s.db.Exec(`UPDATE request_log SET api_key = ? WHERE api_key = ?`, newName, oldName)
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

// ===== 分组管理（groups / group_model_quota）=====

// GroupRow 是 groups 表的行映射。AllowedModels 为 JSON 数组字符串（空/[]=不限）。
type GroupRow struct {
	ID            uint   `json:"id"`
	Name          string `json:"name"`
	AllowedModels string `json:"allowed_models"`
	Remark        string `json:"remark"`
	CreatedAt     string `json:"created_at"`
	// MemberCount 只读：组内 key 数量（列表接口带出）
	MemberCount int `json:"member_count"`
}

// GroupModelQuotaRow 是 group_model_quota 表的行映射（一行 = 一个模型的“组内每人默认”配额）。
// Token 限额单位为原始 token，0 = 该窗口不限。
type GroupModelQuotaRow struct {
	ID         uint   `json:"id"`
	GroupID    uint   `json:"group_id"`
	Model      string `json:"model"` // 模型名；'*' = 全模型兜底
	Token5H    int64  `json:"token_5h"`
	TokenWeek  int64  `json:"token_week"`
	TokenMonth int64  `json:"token_month"`
}

// ListGroups 列出所有组（含成员数）。
func (s *Store) ListGroups() ([]GroupRow, error) {
	rows, err := s.db.Query(`SELECT g.id, g.name, g.allowed_models, g.remark, g.created_at,
		(SELECT COUNT(*) FROM api_tokens t WHERE t.group_id = g.id) AS member_count
		FROM groups g ORDER BY g.id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupRow
	for rows.Next() {
		var g GroupRow
		if err := rows.Scan(&g.ID, &g.Name, &g.AllowedModels, &g.Remark, &g.CreatedAt, &g.MemberCount); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GetGroup 按 id 取组。
func (s *Store) GetGroup(id uint) (*GroupRow, error) {
	var g GroupRow
	err := s.db.QueryRow(`SELECT id, name, allowed_models, remark, created_at FROM groups WHERE id = ?`, id).Scan(
		&g.ID, &g.Name, &g.AllowedModels, &g.Remark, &g.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// CreateGroup 创建组。
func (s *Store) CreateGroup(name, allowedModels, remark string) (*GroupRow, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("name is required")
	}
	if strings.TrimSpace(allowedModels) == "" {
		allowedModels = `[]`
	}
	res, err := s.db.Exec(`INSERT INTO groups (name, allowed_models, remark) VALUES (?, ?, ?)`,
		strings.TrimSpace(name), strings.TrimSpace(allowedModels), remark)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &GroupRow{ID: uint(id), Name: strings.TrimSpace(name), AllowedModels: strings.TrimSpace(allowedModels), Remark: remark, CreatedAt: time.Now().Format(time.RFC3339)}, nil
}

// UpdateGroup 更新组名/白名单/备注。传空字符串的字段表示不修改。
func (s *Store) UpdateGroup(id uint, name, allowedModels, remark string) error {
	q := `UPDATE groups SET `
	var args []interface{}
	add := func(expr string, v interface{}) {
		if len(args) > 0 {
			q += ", "
		}
		q += expr
		args = append(args, v)
	}
	if strings.TrimSpace(name) != "" {
		add("name = ?", strings.TrimSpace(name))
	}
	if allowedModels != "" {
		add("allowed_models = ?", allowedModels)
	}
	if remark != "" {
		add("remark = ?", remark)
	}
	if len(args) == 0 {
		return nil
	}
	q += ` WHERE id = ?`
	args = append(args, id)
	_, err := s.db.Exec(q, args...)
	return err
}

// DeleteGroup 删除组（组内 key 的 group_id 置 0，不删 key）。
func (s *Store) DeleteGroup(id uint) error {
	if _, err := s.db.Exec(`DELETE FROM group_model_quota WHERE group_id = ?`, id); err != nil {
		return err
	}
	if _, err := s.db.Exec(`UPDATE api_tokens SET group_id = 0 WHERE group_id = ?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM groups WHERE id = ?`, id)
	return err
}

// ListGroupQuotas 列出某组的所有模型配额行。
func (s *Store) ListGroupQuotas(groupID uint) ([]GroupModelQuotaRow, error) {
	rows, err := s.db.Query(`SELECT id, group_id, model, token_5h, token_week, token_month
		FROM group_model_quota WHERE group_id = ? ORDER BY id ASC`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupModelQuotaRow
	for rows.Next() {
		var g GroupModelQuotaRow
		if err := rows.Scan(&g.ID, &g.GroupID, &g.Model, &g.Token5H, &g.TokenWeek, &g.TokenMonth); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ListAllGroupQuotas 列出所有组的模型配额（quota 包 Load 用）。
func (s *Store) ListAllGroupQuotas() ([]GroupModelQuotaRow, error) {
	rows, err := s.db.Query(`SELECT id, group_id, model, token_5h, token_week, token_month
		FROM group_model_quota ORDER BY group_id ASC, model ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupModelQuotaRow
	for rows.Next() {
		var g GroupModelQuotaRow
		if err := rows.Scan(&g.ID, &g.GroupID, &g.Model, &g.Token5H, &g.TokenWeek, &g.TokenMonth); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// UpsertGroupQuota 插入/更新某组的一行模型配额（UNIQUE(group_id, model)）。
// token 值为 0 表示删除该行的该窗口限制；若三窗口全 0 则删除整行。
func (s *Store) UpsertGroupQuota(groupID uint, model string, fiveH, week, month int64) error {
	if strings.TrimSpace(model) == "" {
		return fmt.Errorf("model is required")
	}
	if fiveH == 0 && week == 0 && month == 0 {
		// 全 0：无限制，直接删除该行
		_, err := s.db.Exec(`DELETE FROM group_model_quota WHERE group_id = ? AND model = ?`, groupID, strings.TrimSpace(model))
		return err
	}
	_, err := s.db.Exec(`INSERT INTO group_model_quota (group_id, model, token_5h, token_week, token_month)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(group_id, model) DO UPDATE SET token_5h=excluded.token_5h, token_week=excluded.token_week, token_month=excluded.token_month`,
		groupID, strings.TrimSpace(model), fiveH, week, month)
	return err
}

// DeleteGroupQuota 删除某组某模型的配额行。
func (s *Store) DeleteGroupQuota(groupID uint, model string) error {
	_, err := s.db.Exec(`DELETE FROM group_model_quota WHERE group_id = ? AND model = ?`, groupID, model)
	return err
}

// ---- 按天统计预聚合（daily_stats） ----
// 数据量大后实时 GROUP BY 扫 request_log 越来越慢。改为把已完整过去的每一天预聚合到
// daily_stats 一行（date=东八区 YYYY-MM-DD，与 MetricsDaily 分组口径一致）；统计接口只实时
// 查「今天」未纳入统计的部分，历史天从 daily_stats 按主键取。

// DimStat 是单维度（上游/Key/模型）的单日汇总。
type DimStat struct {
	Requests        int64   `json:"requests"`
	Successes       int64   `json:"successes"`
	Tokens          int64   `json:"tokens"`
	Cost            float64 `json:"cost"`
	SumDurationMS   int64   `json:"sum_duration_ms"`
	CacheHitTokens  int64   `json:"cache_hit_tokens"`
	CacheMissTokens int64   `json:"cache_miss_tokens"`
}

// DayStats 是 daily_stats 表的一行。
type DayStats struct {
	Date             string
	Requests         int64
	Successes        int64
	Tokens           int64
	PromptTokens     int64
	CompletionTokens int64
	CacheHitTokens   int64
	CacheMissTokens  int64
	Cost             float64
	SumDurationMS    int64
	ByUpstream       map[string]*DimStat
	ByAPIKey         map[string]*DimStat
	ByModel          map[string]*DimStat
	ComputedAt       string
}

// DayPoint 是某一天的全局汇总（用于趋势图，不含维度拆分）。
type DayPoint struct {
	Date      string  `json:"date"`
	Requests  int64   `json:"requests"`
	Successes int64   `json:"successes"`
	Tokens    int64   `json:"tokens"`
	Cost      float64 `json:"cost"`
}

// RangeAgg 是某个时间范围合并后的统计结果。
type RangeAgg struct {
	Requests         int64
	Successes        int64
	Tokens           int64
	PromptTokens     int64
	CompletionTokens int64
	CacheHitTokens   int64
	CacheMissTokens  int64
	Cost             float64
	SumDurationMS    int64
	ByUpstream       map[string]*DimStat
	ByAPIKey         map[string]*DimStat
	ByModel          map[string]*DimStat
	Days             []DayPoint
}

func newRangeAgg() *RangeAgg {
	return &RangeAgg{
		ByUpstream: map[string]*DimStat{},
		ByAPIKey:   map[string]*DimStat{},
		ByModel:    map[string]*DimStat{},
	}
}

func mergeDim(into map[string]*DimStat, from map[string]*DimStat) {
	for k, v := range from {
		if v == nil {
			continue
		}
		cur, ok := into[k]
		if !ok {
			nc := *v
			into[k] = &nc
			continue
		}
		cur.Requests += v.Requests
		cur.Successes += v.Successes
		cur.Tokens += v.Tokens
		cur.Cost += v.Cost
		cur.SumDurationMS += v.SumDurationMS
		cur.CacheHitTokens += v.CacheHitTokens
		cur.CacheMissTokens += v.CacheMissTokens
	}
}

func (a *RangeAgg) mergeDay(ds *DayStats) {
	if ds == nil {
		return
	}
	a.Requests += ds.Requests
	a.Successes += ds.Successes
	a.Tokens += ds.Tokens
	a.PromptTokens += ds.PromptTokens
	a.CompletionTokens += ds.CompletionTokens
	a.CacheHitTokens += ds.CacheHitTokens
	a.CacheMissTokens += ds.CacheMissTokens
	a.Cost += ds.Cost
	a.SumDurationMS += ds.SumDurationMS
	mergeDim(a.ByUpstream, ds.ByUpstream)
	mergeDim(a.ByAPIKey, ds.ByAPIKey)
	mergeDim(a.ByModel, ds.ByModel)
	a.Days = append(a.Days, DayPoint{Date: ds.Date, Requests: ds.Requests, Successes: ds.Successes, Tokens: ds.Tokens, Cost: ds.Cost})
}

// AggregateDay 实时聚合某东八区日期（YYYY-MM-DD）的全部请求。
// 用 strftime('%Y-%m-%d', ts, '+8 hours')=? 按东八区日期过滤，与 MetricsDaily 口径一致。
func (s *Store) AggregateDay(date string) (*DayStats, error) {
	ds := &DayStats{
		Date:       date,
		ByUpstream: map[string]*DimStat{},
		ByAPIKey:   map[string]*DimStat{},
		ByModel:    map[string]*DimStat{},
	}
	const dayFilter = `strftime('%Y-%m-%d', rl.ts, '+8 hours') = ?`
	// 全局
	if err := s.db.QueryRow(`
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN rl.status < 400 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(rl.tokens), 0),
		       COALESCE(SUM(rl.prompt_tokens), 0),
		       COALESCE(SUM(rl.completion_tokens), 0),
		       COALESCE(SUM(rl.prompt_cache_hit_tokens), 0),
		       COALESCE(SUM(rl.prompt_cache_miss_tokens), 0),
		       COALESCE(SUM(CASE WHEN u.billing_exempt = 1 THEN 0 ELSE rl.cost END), 0),
		       COALESCE(SUM(rl.duration_ms), 0)
		FROM request_log rl LEFT JOIN upstreams u ON u.name = rl.upstream
		WHERE `+dayFilter, date).Scan(
		&ds.Requests, &ds.Successes, &ds.Tokens, &ds.PromptTokens, &ds.CompletionTokens,
		&ds.CacheHitTokens, &ds.CacheMissTokens, &ds.Cost, &ds.SumDurationMS); err != nil {
		return nil, err
	}
	// by_upstream
	if err := s.scanDim(`
		SELECT rl.upstream,
		       COUNT(*),
		       COALESCE(SUM(CASE WHEN rl.status < 400 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(rl.tokens), 0),
		       COALESCE(SUM(CASE WHEN u.billing_exempt = 1 THEN 0 ELSE rl.cost END), 0),
		       COALESCE(SUM(rl.duration_ms), 0),
		       COALESCE(SUM(rl.prompt_cache_hit_tokens), 0),
		       COALESCE(SUM(rl.prompt_cache_miss_tokens), 0)
		FROM request_log rl LEFT JOIN upstreams u ON u.name = rl.upstream
		WHERE `+dayFilter+` GROUP BY rl.upstream`, date, ds.ByUpstream); err != nil {
		return nil, err
	}
	// by_api_key
	if err := s.scanDim(`
		SELECT COALESCE(NULLIF(rl.api_key, ''), (SELECT name FROM api_tokens WHERE enabled=1 ORDER BY id LIMIT 1), '(未标识)'),
		       COUNT(*),
		       COALESCE(SUM(CASE WHEN rl.status < 400 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(rl.tokens), 0),
		       COALESCE(SUM(CASE WHEN u.billing_exempt = 1 THEN 0 ELSE rl.cost END), 0),
		       COALESCE(SUM(rl.duration_ms), 0),
		       COALESCE(SUM(rl.prompt_cache_hit_tokens), 0),
		       COALESCE(SUM(rl.prompt_cache_miss_tokens), 0)
		FROM request_log rl LEFT JOIN upstreams u ON u.name = rl.upstream
		WHERE `+dayFilter+` GROUP BY 1`, date, ds.ByAPIKey); err != nil {
		return nil, err
	}
	// by_model（按 upstream_model，与费用接口一致）
	if err := s.scanDim(`
		SELECT COALESCE(NULLIF(rl.upstream_model, ''), '(未知)'),
		       COUNT(*),
		       COALESCE(SUM(CASE WHEN rl.status < 400 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(rl.tokens), 0),
		       COALESCE(SUM(CASE WHEN u.billing_exempt = 1 THEN 0 ELSE rl.cost END), 0),
		       COALESCE(SUM(rl.duration_ms), 0),
		       COALESCE(SUM(rl.prompt_cache_hit_tokens), 0),
		       COALESCE(SUM(rl.prompt_cache_miss_tokens), 0)
		FROM request_log rl LEFT JOIN upstreams u ON u.name = rl.upstream
		WHERE `+dayFilter+` GROUP BY 1`, date, ds.ByModel); err != nil {
		return nil, err
	}
	ds.ComputedAt = time.Now().Format(time.RFC3339)
	return ds, nil
}

// scanDim 执行按维度聚合查询，把结果填入 out map。
func (s *Store) scanDim(q string, date string, out map[string]*DimStat) error {
	rows, err := s.db.Query(q, date)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		d := &DimStat{}
		if err := rows.Scan(&k, &d.Requests, &d.Successes, &d.Tokens, &d.Cost, &d.SumDurationMS, &d.CacheHitTokens, &d.CacheMissTokens); err != nil {
			continue
		}
		out[k] = d
	}
	return rows.Err()
}

// UpsertDayStats 写入/覆盖某天的预聚合结果。
func (s *Store) UpsertDayStats(ds *DayStats) error {
	bu, _ := json.Marshal(ds.ByUpstream)
	bk, _ := json.Marshal(ds.ByAPIKey)
	bm, _ := json.Marshal(ds.ByModel)
	_, err := s.db.Exec(`
		INSERT INTO daily_stats (date, requests, successes, tokens, prompt_tokens, completion_tokens,
		                        cache_hit_tokens, cache_miss_tokens, cost, sum_duration_ms,
		                        by_upstream, by_api_key, by_model, computed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(date) DO UPDATE SET
			requests=excluded.requests, successes=excluded.successes, tokens=excluded.tokens,
			prompt_tokens=excluded.prompt_tokens, completion_tokens=excluded.completion_tokens,
			cache_hit_tokens=excluded.cache_hit_tokens, cache_miss_tokens=excluded.cache_miss_tokens,
			cost=excluded.cost, sum_duration_ms=excluded.sum_duration_ms,
			by_upstream=excluded.by_upstream, by_api_key=excluded.by_api_key,
			by_model=excluded.by_model, computed_at=excluded.computed_at`,
		ds.Date, ds.Requests, ds.Successes, ds.Tokens, ds.PromptTokens, ds.CompletionTokens,
		ds.CacheHitTokens, ds.CacheMissTokens, ds.Cost, ds.SumDurationMS,
		string(bu), string(bk), string(bm), ds.ComputedAt)
	return err
}

// GetDayStats 按日期列表批量读取预聚合结果（缺的日期不返回）。
func (s *Store) GetDayStats(dates []string) ([]*DayStats, error) {
	if len(dates) == 0 {
		return nil, nil
	}
	ph := strings.Repeat("?,", len(dates))
	ph = ph[:len(ph)-1]
	args := make([]any, len(dates))
	for i, d := range dates {
		args[i] = d
	}
	rows, err := s.db.Query(`SELECT date, requests, successes, tokens, prompt_tokens, completion_tokens,
		cache_hit_tokens, cache_miss_tokens, cost, sum_duration_ms, by_upstream, by_api_key, by_model, computed_at
		FROM daily_stats WHERE date IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*DayStats
	for rows.Next() {
		ds := &DayStats{ByUpstream: map[string]*DimStat{}, ByAPIKey: map[string]*DimStat{}, ByModel: map[string]*DimStat{}}
		var bu, bk, bm string
		if err := rows.Scan(&ds.Date, &ds.Requests, &ds.Successes, &ds.Tokens, &ds.PromptTokens, &ds.CompletionTokens,
			&ds.CacheHitTokens, &ds.CacheMissTokens, &ds.Cost, &ds.SumDurationMS, &bu, &bk, &bm, &ds.ComputedAt); err != nil {
			continue
		}
		_ = json.Unmarshal([]byte(bu), &ds.ByUpstream)
		_ = json.Unmarshal([]byte(bk), &ds.ByAPIKey)
		_ = json.Unmarshal([]byte(bm), &ds.ByModel)
		out = append(out, ds)
	}
	return out, rows.Err()
}

// EarliestLogDate 返回 request_log 中东八区最早日期（YYYY-MM-DD），无数据返回 ""。
func (s *Store) EarliestLogDate() string {
	var d string
	if err := s.db.QueryRow(`SELECT MIN(strftime('%Y-%m-%d', ts, '+8 hours')) FROM request_log`).Scan(&d); err != nil || d == "" {
		return ""
	}
	return d
}

// EarliestStatDate 返回统计可覆盖的最早日期（东八区 YYYY-MM-DD）。
// 优先取 daily_stats 已聚合的最早天（即使 request_log 明细已按保留期清理，
// 历史统计仍完整可见）；无 daily_stats 时回退 request_log 最早记录。
// 用于 AggregateRange("all") 的范围起点。
func (s *Store) EarliestStatDate() string {
	var d string
	if err := s.db.QueryRow(`SELECT MIN(date) FROM daily_stats`).Scan(&d); err == nil && d != "" {
		return d
	}
	return s.EarliestLogDate()
}

// RebuildDailyStats 全量重算历史每一天的预聚合（幂等 upsert），供后台/手动刷新调用。
func (s *Store) RebuildDailyStats() error {
	ed := s.EarliestLogDate()
	if ed == "" {
		return nil
	}
	loc := time.FixedZone("CST", 8*3600)
	start, err := time.ParseInLocation("2006-01-02", ed, loc)
	if err != nil {
		return err
	}
	today := time.Now().In(loc)
	n := 0
	for d := start; !d.After(today); d = d.AddDate(0, 0, 1) {
		ds, err := s.AggregateDay(d.Format("2006-01-02"))
		if err != nil {
			slog.Error("rebuild daily stats", "date", d.Format("2006-01-02"), "error", err)
			continue
		}
		if err := s.UpsertDayStats(ds); err != nil {
			slog.Error("upsert daily stats", "date", d.Format("2006-01-02"), "error", err)
			continue
		}
		n++
	}
	slog.Info("rebuild daily stats done", "days", n)
	return nil
}

// AggregateRange 聚合某个 range（today|3d|7d|30d|all）的全部请求。
// 历史天从 daily_stats 按主键取（快）；缺失的天回退实时聚合（保证正确）；今天始终实时聚合。
func (s *Store) AggregateRange(rangeStr string, loc *time.Location) (*RangeAgg, error) {
	now := time.Now().In(loc)
	var start time.Time
	switch rangeStr {
	case "today":
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	case "3d":
		start = now.AddDate(0, 0, -3)
	case "7d":
		start = now.AddDate(0, 0, -7)
	case "30d":
		start = now.AddDate(0, 0, -30)
	case "all", "":
		ed := s.EarliestStatDate()
		if ed == "" {
			return newRangeAgg(), nil
		}
		st, err := time.ParseInLocation("2006-01-02", ed, loc)
		if err != nil {
			return nil, err
		}
		start = st
	default:
		start = now.AddDate(0, 0, -7)
	}
	var dates []string
	for d := start; !d.After(now); d = d.AddDate(0, 0, 1) {
		dates = append(dates, d.Format("2006-01-02"))
	}
	if len(dates) == 0 {
		return newRangeAgg(), nil
	}
	histDates := dates[:len(dates)-1]
	todayOnly := dates[len(dates)-1]

	agg := newRangeAgg()
	have := map[string]*DayStats{}
	if len(histDates) > 0 {
		if hist, err := s.GetDayStats(histDates); err == nil {
			for _, ds := range hist {
				have[ds.Date] = ds
			}
		}
	}
	for _, d := range histDates {
		var ds *DayStats
		if h, ok := have[d]; ok {
			ds = h
		} else {
			ds, _ = s.AggregateDay(d)
		}
		agg.mergeDay(ds)
	}
	todayDS, err := s.AggregateDay(todayOnly)
	if err != nil {
		return nil, err
	}
	agg.mergeDay(todayDS)
	return agg, nil
}
