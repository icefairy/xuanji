package store

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// openTestStore 在临时目录打开一个 store 供测试使用。
func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// countRows 返回 request_log 表当前行数。
func countRows(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM request_log`).Scan(&n); err != nil {
		t.Fatalf("COUNT(*): %v", err)
	}
	return n
}

// sampleRecord 构造一条最小可用测试记录。
func sampleRecord(i int) Record {
	return Record{
		Timestamp:        time.Date(2026, 8, 1, 12, 0, i, 0, time.UTC),
		Upstream:         "up-" + strconv.Itoa(i%3),
		Model:            "model-x",
		Endpoint:         "chat",
		Status:           200,
		DurationMS:       12,
		PromptTokens:     10,
		CompletionTokens: 24,
		Tokens:           34,
	}
}

func TestOpen_CreateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "xuanji.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// 数据文件应已创建
	if _, err := filepath.Glob(path); err != nil {
		t.Fatalf("glob db file: %v", err)
	}

	// request_log 表应存在
	var name string
	err = s.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='request_log'`,
	).Scan(&name)
	if err == sql.ErrNoRows {
		t.Fatal("request_log table not found")
	} else if err != nil {
		t.Fatalf("query table: %v", err)
	}

	// 两个索引也应存在
	for _, idx := range []string{"idx_request_log_ts", "idx_request_log_upstream"} {
		var iname string
		err = s.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, idx,
		).Scan(&iname)
		if err == sql.ErrNoRows {
			t.Errorf("index %s not found", idx)
		} else if err != nil {
			t.Fatalf("query index %s: %v", idx, err)
		}
	}
}

func TestInsertAndBatch(t *testing.T) {
	s := openTestStore(t)

	// 单条插入
	if err := s.Insert(sampleRecord(1)); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// 批量插入（事务）
	recs := make([]Record, 0, 5)
	for i := 0; i < 5; i++ {
		recs = append(recs, sampleRecord(i))
	}
	if err := s.InsertBatch(recs); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	if got := countRows(t, s); got != 6 {
		t.Errorf("rows = %d, want 6", got)
	}

	// 空批量不报错
	if err := s.InsertBatch(nil); err != nil {
		t.Fatalf("InsertBatch(nil): %v", err)
	}
}

func TestRecorder_AsyncWrite(t *testing.T) {
	s := openTestStore(t)
	r := NewRecorder(s)

	for i := 0; i < 10; i++ {
		r.Record(sampleRecord(i))
	}

	// Close 会 flush 剩余记录，10 条都应落库
	r.Close()
	if got := countRows(t, s); got != 10 {
		t.Errorf("rows after Close = %d, want 10", got)
	}
}

func TestRecorder_ChannelFullNoBlock(t *testing.T) {
	s := openTestStore(t)
	// 小缓冲 + 超长周期：后台协程不会在测试期间主动刷盘
	r := newRecorder(s, 3, time.Hour)

	// 先关闭后台协程，保证 channel 不再被 drain
	r.Close()

	// 塞满 channel（缓冲 3）
	for i := 0; i < 3; i++ {
		r.Record(sampleRecord(i))
	}

	// 第 4 条应被丢弃而非阻塞
	done := make(chan struct{})
	go func() {
		r.Record(sampleRecord(99))
		close(done)
	}()

	select {
	case <-done:
		// 未阻塞，符合预期
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Record blocked when channel full")
	}
}

func TestConfigCRUD(t *testing.T) {
	s := openTestStore(t)

	// 不存在的 key 返回空串
	if v, err := s.GetConfig("server.port"); err != nil || v != "" {
		t.Errorf("GetConfig(missing) = %q, %v; want empty, nil", v, err)
	}

	// 写入
	if err := s.SetConfig("server.port", "8787"); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if v, err := s.GetConfig("server.port"); err != nil || v != "8787" {
		t.Errorf("GetConfig = %q, %v; want 8787, nil", v, err)
	}

	// 更新（存在则覆盖）
	if err := s.SetConfig("server.port", "9000"); err != nil {
		t.Fatalf("SetConfig update: %v", err)
	}
	if v, _ := s.GetConfig("server.port"); v != "9000" {
		t.Errorf("GetConfig after update = %q, want 9000", v)
	}

	// GetAllConfig 返回全部
	if err := s.SetConfig("retry.max_retries", "3"); err != nil {
		t.Fatalf("SetConfig retry: %v", err)
	}
	all, err := s.GetAllConfig()
	if err != nil {
		t.Fatalf("GetAllConfig: %v", err)
	}
	if all["server.port"] != "9000" || all["retry.max_retries"] != "3" {
		t.Errorf("GetAllConfig = %v, want server.port=9000 retry.max_retries=3", all)
	}
}

func TestSeedDefaults(t *testing.T) {
	s := openTestStore(t)

	if err := s.SeedDefaults(); err != nil {
		t.Fatalf("SeedDefaults: %v", err)
	}
	all, err := s.GetAllConfig()
	if err != nil {
		t.Fatalf("GetAllConfig: %v", err)
	}
	// 17 个既有默认值 + 1 个 proxy.debug_request_log（异常请求诊断日志，默认关闭）。
	if len(all) != 18 {
		t.Errorf("defaults count = %d, want 18 (%v)", len(all), all)
	}
	if all["proxy.debug_request_log"] != "false" {
		t.Errorf("proxy.debug_request_log = %q, want false（默认关闭）", all["proxy.debug_request_log"])
	}
	if all["server.port"] != "8787" {
		t.Errorf("server.port = %q, want 8787", all["server.port"])
	}
	if all["retry.max_retries"] != "3" {
		t.Errorf("retry.max_retries = %q, want 3", all["retry.max_retries"])
	}
	if all["retry.fast_fail_minutes"] != "5" {
		t.Errorf("retry.fast_fail_minutes = %q, want 5", all["retry.fast_fail_minutes"])
	}
	if all["retry.fast_fail_probe_minutes"] != "5" {
		t.Errorf("retry.fast_fail_probe_minutes = %q, want 5", all["retry.fast_fail_probe_minutes"])
	}
	if all["retry.upstream_timeout"] != "60" {
		t.Errorf("retry.upstream_timeout = %q, want 60", all["retry.upstream_timeout"])
	}
	// 新增 proxy 配置项（视频透传、per-key 冷却、最佳思考等级）
	if all["proxy.video_pass_through"] != "false" {
		t.Errorf("proxy.video_pass_through = %q, want false", all["proxy.video_pass_through"])
	}
	if all["proxy.cooldown_seconds"] != "1" {
		t.Errorf("proxy.cooldown_seconds = %q, want 1", all["proxy.cooldown_seconds"])
	}
	if all["proxy.cooldown_upstreams"] != "[\"商汤\"]" {
		t.Errorf("proxy.cooldown_upstreams = %q, want [\"商汤\"]", all["proxy.cooldown_upstreams"])
	}
	if all["proxy.auto_best_effort"] != "false" {
		t.Errorf("proxy.auto_best_effort = %q, want false", all["proxy.auto_best_effort"])
	}
	// reasoning_content 回传缓存（DeepSeek thinking 模式 tool-calling 兼容，默认开）
	if all["proxy.cache_reasoning_content"] != "true" {
		t.Errorf("proxy.cache_reasoning_content = %q, want true", all["proxy.cache_reasoning_content"])
	}
	// 客户端程序分析（默认关，间隔默认 600 秒/10 分钟）
	if all["proxy.client_analysis"] != "false" {
		t.Errorf("proxy.client_analysis = %q, want false", all["proxy.client_analysis"])
	}
	if all["proxy.client_analysis_interval"] != "600" {
		t.Errorf("proxy.client_analysis_interval = %q, want 600", all["proxy.client_analysis_interval"])
	}
	// 上游健康告警（默认关：webhook_url 空串 + 连续失败阈值 3）
	if all["alert.webhook_url"] != "" {
		t.Errorf("alert.webhook_url = %q, want empty", all["alert.webhook_url"])
	}
	if all["alert.consecutive_fails"] != "3" {
		t.Errorf("alert.consecutive_fails = %q, want 3", all["alert.consecutive_fails"])
	}

	// 已有数据时不覆盖
	if err := s.SetConfig("server.port", "9999"); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if err := s.SeedDefaults(); err != nil {
		t.Fatalf("SeedDefaults second: %v", err)
	}
	if v, _ := s.GetConfig("server.port"); v != "9999" {
		t.Errorf("SeedDefaults overwrote existing server.port = %q, want 9999", v)
	}
}

// TestDeleteUpstream_CleansRuleReferences 验证删除上游时同步清理路由规则中的引用，
// 避免规则残留已删除上游名（孤儿引用）导致页面显示"删不掉"的上游。
func TestDeleteUpstream_CleansRuleReferences(t *testing.T) {
	s := openTestStore(t)

	// 创建两个上游
	for _, name := range []string{"up-a", "up-b"} {
		if err := s.CreateUpstream(&UpstreamRow{
			Name: name, Type: "openai", BaseURL: "http://x/" + name,
			Tier: "free", Priority: 10, Weight: 1,
		}); err != nil {
			t.Fatalf("CreateUpstream(%s): %v", name, err)
		}
	}
	// 规则 1 引用 up-a + up-b；规则 2 只引用 up-a
	if err := s.CreateRoutingRule(&RoutingRuleRow{
		Model: "model-x", Strategy: "primary_backup",
		Upstreams: `["up-a","up-b"]`,
	}); err != nil {
		t.Fatalf("CreateRoutingRule 1: %v", err)
	}
	if err := s.CreateRoutingRule(&RoutingRuleRow{
		Model: "model-y", Strategy: "",
		Upstreams: `["up-a"]`,
	}); err != nil {
		t.Fatalf("CreateRoutingRule 2: %v", err)
	}

	// 删除 up-a：upstreams 表删除 + 两个规则里的引用都应被清理
	if err := s.DeleteUpstream("up-a"); err != nil {
		t.Fatalf("DeleteUpstream: %v", err)
	}

	// upstreams 表里不应再有 up-a
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM upstreams WHERE name='up-a'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("upstreams 表仍存在 up-a (n=%d, err=%v)", n, err)
	}
	// 规则 1 只剩 up-b
	rows, err := s.ListRoutingRules()
	if err != nil {
		t.Fatalf("ListRoutingRules: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rules count = %d, want 2", len(rows))
	}
	for _, r := range rows {
		if r.Upstreams == `["up-a","up-b"]` {
			t.Errorf("规则 %s 仍引用已删除的 up-a: %s", r.Model, r.Upstreams)
		}
		if r.Model == "model-x" && r.Upstreams != `["up-b"]` {
			t.Errorf("规则 model-x upstreams = %s, want [\"up-b\"]", r.Upstreams)
		}
		if r.Model == "model-y" && r.Upstreams != `[]` {
			t.Errorf("规则 model-y upstreams = %s, want []", r.Upstreams)
		}
	}

	// 删除不存在的上游不应报错（幂等）
	if err := s.DeleteUpstream("no-such-upstream"); err != nil {
		t.Errorf("DeleteUpstream(nonexistent) = %v, want nil", err)
	}
}

// TestClientProfiles 验证 client_profiles 表的 upsert/列表与去重 addr 聚合。

// columnNames 返回表的列名列表。
func columnNames(t *testing.T, s *Store, table string) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, name)
	}
	return out
}

// contains 判断切片是否含目标字符串。
func contains(list []string, target string) bool {
	for _, s := range list {
		if s == target {
			return true
		}
	}
	return false
}

// TestOpen_OldDBAddsVisionColumns 老库（routing_rules 无 vision / vision_fallback 列）
// 打开后应自动补列（幂等迁移），且 CRUD 正常读写新字段。
func TestOpen_OldDBAddsVisionColumns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	// 用旧版建表语句模拟老库（无 vision 列）
	if _, err := raw.Exec(`CREATE TABLE routing_rules (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		model      TEXT    NOT NULL UNIQUE,
		strategy   TEXT    NOT NULL DEFAULT '',
		upstreams  TEXT    NOT NULL DEFAULT '[]',
		created_at TEXT    NOT NULL DEFAULT (datetime('now')),
		updated_at TEXT    NOT NULL DEFAULT (datetime('now'))
	);`); err != nil {
		t.Fatalf("create old table: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO routing_rules (model, strategy, upstreams) VALUES ('old-model', 'primary_backup', '["up"]')`); err != nil {
		t.Fatalf("insert old row: %v", err)
	}
	raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open old db: %v", err)
	}
	defer s.Close()

	// 新列已自动补上
	cols := columnNames(t, s, "routing_rules")
	if !contains(cols, "vision") || !contains(cols, "vision_fallback") {
		t.Fatalf("migration missing columns, got %v", cols)
	}

	// 旧数据读取正常，新字段为默认值
	r, err := s.GetRoutingRule("old-model")
	if err != nil {
		t.Fatalf("GetRoutingRule(old-model): %v", err)
	}
	if r.Vision != 0 || r.VisionFallback != "" {
		t.Errorf("old row defaults = vision:%d fallback:%q, want 0/''", r.Vision, r.VisionFallback)
	}

	// 新建含新字段的规则可写入读回
	if err := s.CreateRoutingRule(&RoutingRuleRow{Model: "new-model", Strategy: "primary_backup", Upstreams: `["up"]`, Vision: 1, VisionFallback: "flash"}); err != nil {
		t.Fatalf("CreateRoutingRule: %v", err)
	}
	r2, err := s.GetRoutingRule("new-model")
	if err != nil {
		t.Fatalf("GetRoutingRule(new-model): %v", err)
	}
	if r2.Vision != 1 || r2.VisionFallback != "flash" {
		t.Errorf("new row = vision:%d fallback:%q, want 1/'flash'", r2.Vision, r2.VisionFallback)
	}

	// 更新也生效
	if err := s.UpdateRoutingRule("new-model", &RoutingRuleRow{Model: "new-model", Strategy: "latency", Upstreams: `["up2"]`, Vision: 0, VisionFallback: ""}); err != nil {
		t.Fatalf("UpdateRoutingRule: %v", err)
	}
	r3, _ := s.GetRoutingRule("new-model")
	if r3.Vision != 0 || r3.VisionFallback != "" {
		t.Errorf("updated row = vision:%d fallback:%q, want 0/''", r3.Vision, r3.VisionFallback)
	}

	// ListRoutingRules 也带出新字段
	all, err := s.ListRoutingRules()
	if err != nil {
		t.Fatalf("ListRoutingRules: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("rules count = %d, want 2", len(all))
	}
}

// TestOpen_OldDBAddsKindColumn 验证旧库（无 kind 列的 upstreams 表）自动迁移：
// Open 时 ensureColumn 幂等补上 kind 列，旧数据读取正常（Kind 为空串，LoadFromDB 会补默认 chat）。
func TestOpen_OldDBAddsKindColumn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	// 用旧版建表语句模拟老库（无 kind 列）
	if _, err := raw.Exec(`CREATE TABLE upstreams (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		name          TEXT    NOT NULL UNIQUE,
		type          TEXT    NOT NULL DEFAULT '',
		base_url      TEXT    NOT NULL DEFAULT '',
		api_key       TEXT    NOT NULL DEFAULT '',
		tier          TEXT    NOT NULL DEFAULT '',
		priority      INTEGER NOT NULL DEFAULT 0,
		weight        INTEGER NOT NULL DEFAULT 0,
		models        TEXT    NOT NULL DEFAULT '[]',
		model_mapping TEXT    NOT NULL DEFAULT '{}',
		created_at    TEXT    NOT NULL DEFAULT (datetime('now')),
		updated_at    TEXT    NOT NULL DEFAULT (datetime('now'))
	);`); err != nil {
		t.Fatalf("create old table: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO upstreams (name, type, base_url, api_key, models) VALUES ('legacy-up', 'openai', 'http://x/v1', 'k', '["m1"]')`); err != nil {
		t.Fatalf("insert old row: %v", err)
	}
	raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open old db: %v", err)
	}
	defer s.Close()

	// kind 列已自动补上
	cols := columnNames(t, s, "upstreams")
	if !contains(cols, "kind") {
		t.Fatalf("migration missing kind column, got %v", cols)
	}

	// 旧数据读取正常，kind 为默认空串
	u, err := s.GetUpstream("legacy-up")
	if err != nil {
		t.Fatalf("GetUpstream(legacy-up): %v", err)
	}
	if u.Kind != "" {
		t.Errorf("old row kind = %q, want empty (加载时补默认 chat)", u.Kind)
	}
}

func TestDeleteConfig(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetConfig("test.key.a", "val-a"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if v, _ := s.GetConfig("test.key.a"); v != "val-a" {
		t.Fatalf("pre-delete value mismatch")
	}
	if err := s.DeleteConfig("test.key.a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if v, _ := s.GetConfig("test.key.a"); v != "" {
		t.Fatalf("post-delete should be empty, got %q", v)
	}
	// 已删除的 key 再删应静默成功
	if err := s.DeleteConfig("test.key.a"); err != nil {
		t.Fatalf("re-delete should be no-op, got: %v", err)
	}
}

func TestDeleteConfigProtectsCriticalKeys(t *testing.T) {
	s := newTestStore(t)
	critical := []string{
		"server.port", "admin.api_key", "admin.jwt_secret",
	}
	for _, k := range critical {
		if err := s.DeleteConfig(k); err == nil {
			t.Errorf("应拒绝删除关键键 %q", k)
		}
	}
	// 业务键允许删除（包括 retry.*、proxy.*、upstream.* 等）
	safe := []string{"retry.upstream_timeout", "proxy.auto_best_effort", "upstream.foo.bar"}
	for _, k := range safe {
		if err := s.SetConfig(k, "v"); err != nil {
			t.Fatalf("pre-set %q: %v", k, err)
		}
		if err := s.DeleteConfig(k); err != nil {
			t.Errorf("应允许删除业务键 %q, got: %v", k, err)
		}
	}
}

// TestDeleteUpstreams_Batch 验证批量删除：一次事务删多个上游，级联清理规则引用、
// 请求日志、上游衍生配置键（upstream.<name>.*）、模型级欠费与 token 上限。
func TestDeleteUpstreams_Batch(t *testing.T) {
	s := openTestStore(t)

	for _, name := range []string{"up-a", "up-b", "up-c"} {
		if err := s.CreateUpstream(&UpstreamRow{
			Name: name, Type: "openai", BaseURL: "http://x/" + name,
			Tier: "free", Priority: 10, Weight: 1,
		}); err != nil {
			t.Fatalf("CreateUpstream(%s): %v", name, err)
		}
	}
	// 规则 1 引用 a+b+c，规则 2 只引用 c（不应被波及）
	if err := s.CreateRoutingRule(&RoutingRuleRow{
		Model: "model-x", Strategy: "primary_backup",
		Upstreams: `["up-a","up-b","up-c"]`,
	}); err != nil {
		t.Fatalf("CreateRoutingRule 1: %v", err)
	}
	if err := s.CreateRoutingRule(&RoutingRuleRow{
		Model: "model-y", Upstreams: `["up-c"]`,
	}); err != nil {
		t.Fatalf("CreateRoutingRule 2: %v", err)
	}
	// 每个上游各有一条衍生配置键
	for _, name := range []string{"up-a", "up-b", "up-c"} {
		if err := s.SetConfig("upstream."+name+".per_model_billing", "1"); err != nil {
			t.Fatal(err)
		}
	}
	// 被删上游的模型级欠费与 token 上限记录
	if err := s.SetModelArrears("up-a", "model-x", "余额不足"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTokenLimit("up-b", "model-x", 100, 200, "error_message"); err != nil {
		t.Fatal(err)
	}
	// 请求日志：一条只涉及 up-a，一条涉及 up-a+up-c（逗号分隔）
	if err := s.Insert(Record{
		Timestamp: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
		Upstream:  "up-a", Model: "model-x", Endpoint: "chat", Status: 200,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(Record{
		Timestamp: time.Date(2026, 8, 1, 12, 0, 1, 0, time.UTC),
		Upstream:  "up-a,up-c", Model: "model-x", Endpoint: "chat", Status: 200,
	}); err != nil {
		t.Fatal(err)
	}

	// 批量删除 a、b（重复与空名应被忽略），c 保留
	n, err := s.DeleteUpstreams([]string{"up-a", "up-b", "up-a", "", "  "})
	if err != nil {
		t.Fatalf("DeleteUpstreams: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted = %d, want 2", n)
	}

	// 剩余上游只有 up-c
	rows, err := s.ListUpstreams()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "up-c" {
		t.Fatalf("remaining upstreams = %+v, want [up-c]", rows)
	}

	// 规则引用：model-x 只剩 c；model-y 不受影响
	rules, err := s.ListRoutingRules()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rules {
		if r.Model == "model-x" && r.Upstreams != `["up-c"]` {
			t.Errorf("model-x upstreams = %s, want [\"up-c\"]", r.Upstreams)
		}
		if r.Model == "model-y" && r.Upstreams != `["up-c"]` {
			t.Errorf("model-y upstreams = %s, want [\"up-c\"]（未被删除的上游不应被动）", r.Upstreams)
		}
	}

	// 衍生配置键：a/b 清掉，c 保留
	for _, k := range []string{"upstream.up-a.per_model_billing", "upstream.up-b.per_model_billing"} {
		if v, _ := s.GetConfig(k); v != "" {
			t.Errorf("%s 应随上游删除被清理，仍为 %q", k, v)
		}
	}
	if v, _ := s.GetConfig("upstream.up-c.per_model_billing"); v != "1" {
		t.Errorf("upstream.up-c.per_model_billing = %q, want 1（保留上游的键不应被删）", v)
	}

	// 欠费与 token 上限记录
	if s.IsModelArrearRecorded("up-a", "model-x") {
		t.Error("up-a 的模型级欠费记录应随上游删除被清理")
	}
	if lim, _ := s.GetTokenLimit("up-b", "model-x"); lim != nil {
		t.Errorf("up-b 的 token 上限记录应随上游删除被清理，got %+v", lim)
	}

	// 请求日志：up-a 独占行删除；up-a,up-c 混合行整行删除（与单条删除语义一致）
	if got := countRows(t, s); got != 0 {
		t.Errorf("request_log rows = %d, want 0", got)
	}

	// 空列表 / 全不存在：幂等无副作用
	if n, err := s.DeleteUpstreams(nil); err != nil || n != 0 {
		t.Errorf("DeleteUpstreams(nil) = (%d, %v), want (0, nil)", n, err)
	}
	if n, err := s.DeleteUpstreams([]string{"no-such"}); err != nil || n != 0 {
		t.Errorf("DeleteUpstreams(nonexistent) = (%d, %v), want (0, nil)", n, err)
	}
}

// TestDeleteUpstreams_EscapesLikeWildcard 验证上游名含 LIKE 通配符（% _）时，
// 衍生配置键的清理不会误伤其他上游的键。
func TestDeleteUpstreams_EscapesLikeWildcard(t *testing.T) {
	s := openTestStore(t)

	// up_a 与 upXa：'_' 在 LIKE 里是单字符通配符，若未转义会同时命中 upXa 的键
	for _, name := range []string{"up_a", "upXa"} {
		if err := s.CreateUpstream(&UpstreamRow{
			Name: name, Type: "openai", BaseURL: "http://x/" + name, Tier: "free",
		}); err != nil {
			t.Fatalf("CreateUpstream(%s): %v", name, err)
		}
		if err := s.SetConfig("upstream."+name+".per_model_billing", "1"); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := s.DeleteUpstreams([]string{"up_a"}); err != nil {
		t.Fatalf("DeleteUpstreams: %v", err)
	}
	if v, _ := s.GetConfig("upstream.up_a.per_model_billing"); v != "" {
		t.Errorf("up_a 的键应被清理，仍为 %q", v)
	}
	if v, _ := s.GetConfig("upstream.upXa.per_model_billing"); v != "1" {
		t.Errorf("upXa 的键被误删（LIKE 通配符未转义），got %q", v)
	}
}

// TestDeleteConfigs_Batch 验证批量删配置：系统关键键整体拒绝、普通键成批删除。
func TestDeleteConfigs_Batch(t *testing.T) {
	s := openTestStore(t)
	for _, k := range []string{"upstream.ghost.per_model_billing", "retry.upstream_timeout", "alert.webhook_url"} {
		if err := s.SetConfig(k, "1"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetConfig("server.port", "8787"); err != nil {
		t.Fatal(err)
	}

	// 含关键键 → 整体拒绝，一条都不删
	if n, err := s.DeleteConfigs([]string{"upstream.ghost.per_model_billing", "server.port"}); err == nil {
		t.Errorf("含 server.port 的批量删除应被拒绝，got n=%d, err=nil", n)
	}
	if v, _ := s.GetConfig("upstream.ghost.per_model_billing"); v != "1" {
		t.Errorf("整体拒绝时不应删掉任何键，got %q", v)
	}

	// 普通键批量删除（重复与空名忽略）
	n, err := s.DeleteConfigs([]string{
		"upstream.ghost.per_model_billing", "retry.upstream_timeout",
		"upstream.ghost.per_model_billing", "", "no-such-key",
	})
	if err != nil {
		t.Fatalf("DeleteConfigs: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted = %d, want 2", n)
	}
	if v, _ := s.GetConfig("upstream.ghost.per_model_billing"); v != "" {
		t.Errorf("键应已删除，got %q", v)
	}
	if v, _ := s.GetConfig("alert.webhook_url"); v != "1" {
		t.Errorf("未列入删除的键不应被动，got %q", v)
	}
}

// TestOrphanUpstreamConfigKeys 验证孤儿配置键扫描：只报上游已不存在的 upstream.<name>.*。
func TestOrphanUpstreamConfigKeys(t *testing.T) {
	s := openTestStore(t)
	if err := s.CreateUpstream(&UpstreamRow{
		Name: "alive", Type: "openai", BaseURL: "http://x", Tier: "free",
	}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{
		"upstream.alive.per_model_billing",
		"upstream.ghost.per_model_billing",
		"upstream.ghost.vendor",
		"retry.upstream_timeout", // 非 upstream.* 前缀，不应误报
	} {
		if err := s.SetConfig(k, "1"); err != nil {
			t.Fatal(err)
		}
	}

	orphans, err := s.OrphanUpstreamConfigKeys()
	if err != nil {
		t.Fatalf("OrphanUpstreamConfigKeys: %v", err)
	}
	if len(orphans) != 1 {
		t.Fatalf("orphans = %+v, want 只有 ghost 一组", orphans)
	}
	got := orphans["ghost"]
	if len(got) != 2 {
		t.Fatalf("ghost keys = %v, want 2 条", got)
	}
	if _, ok := orphans["alive"]; ok {
		t.Error("存活上游的键不应被列为孤儿")
	}
}

// newTestStore 建立临时库并返回 Store（测试结束自动清理）。
// 原在 vendor_accounts_test.go（已随厂商账号池体系删除）。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
