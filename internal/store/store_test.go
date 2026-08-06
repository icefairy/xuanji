package store

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
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
	if len(all) != 15 {
		t.Errorf("defaults count = %d, want 15 (%v)", len(all), all)
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
func TestClientProfiles(t *testing.T) {
	s := openTestStore(t)

	// 1. upsert 新增
	if err := s.UpsertClientProfile(ClientProfile{
		ClientAddr: "192.168.1.20:53211",
		Program:    "Hermes",
		Confidence: 0.8,
		Evidence:   "UA=python-requests, 端口53211",
	}); err != nil {
		t.Fatalf("UpsertClientProfile: %v", err)
	}
	// 2. 同 addr 再次 upsert 更新（不新增行）
	if err := s.UpsertClientProfile(ClientProfile{
		ClientAddr: "192.168.1.20:53211",
		Program:    "OpenCode",
		Confidence: 0.9,
		Evidence:   "更新后的证据",
	}); err != nil {
		t.Fatalf("UpsertClientProfile update: %v", err)
	}
	list, err := s.ListClientProfiles()
	if err != nil {
		t.Fatalf("ListClientProfiles: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("profiles count = %d, want 1 (upsert 应去重)", len(list))
	}
	if list[0].Program != "OpenCode" || list[0].Confidence != 0.9 {
		t.Errorf("profile = %+v, want updated program=OpenCode confidence=0.9", list[0])
	}

	// 3. 写入两条日志，验证 GetDistinctClientAddrs 与 GetClientAddrFeatures
	now := time.Now()
	for i, rec := range []Record{
		{Timestamp: now, Upstream: "up", Model: "deepseek-v4-flash", Endpoint: "chat", Status: 200, ClientAddr: "192.168.1.20:53211", UserAgent: "Hermes/0.5.2"},
		{Timestamp: now, Upstream: "up", Model: "deepseek-v4-flash", Endpoint: "chat", Status: 200, ClientAddr: "192.168.1.21:6000", UserAgent: "claude-cli/1.0.66"},
		{Timestamp: now, Upstream: "up", Model: "gpt-5", Endpoint: "chat", Status: 200, ClientAddr: "192.168.1.20:53211", UserAgent: "Hermes/0.5.2"},
	} {
		if err := s.Insert(rec); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	addrs, err := s.GetDistinctClientAddrs(10)
	if err != nil {
		t.Fatalf("GetDistinctClientAddrs: %v", err)
	}
	if len(addrs) != 2 {
		t.Errorf("distinct addrs = %v, want 2", addrs)
	}
	feats, err := s.GetClientAddrFeatures(10)
	if err != nil {
		t.Fatalf("GetClientAddrFeatures: %v", err)
	}
	if len(feats) != 2 {
		t.Fatalf("features count = %d, want 2", len(feats))
	}
	for _, f := range feats {
		if f.ClientAddr == "192.168.1.20:53211" {
			if f.Requests != 2 {
				t.Errorf("addr 192.168.1.20 requests = %d, want 2", f.Requests)
			}
			if f.Models != "deepseek-v4-flash,gpt-5" && f.Models != "gpt-5,deepseek-v4-flash" {
				t.Errorf("addr models = %q, want deepseek-v4-flash,gpt-5", f.Models)
			}
			// UA 聚合：两条同 addr 日志 UA 相同，DISTINCT 后应为单值
			if f.UserAgents != "Hermes/0.5.2" {
				t.Errorf("addr 192.168.1.20 user_agents = %q, want Hermes/0.5.2", f.UserAgents)
			}
		}
	}
}

// TestRecorder_UserAgentTruncation 验证 Recorder.Record 对超长 UA 截断 200 字符。
func TestRecorder_UserAgentTruncation(t *testing.T) {
	s := openTestStore(t)
	r := newRecorder(s, 100, time.Hour) // 不自动刷盘，直接检查 channel 里的记录
	defer r.Close()

	longUA := strings.Repeat("Mozilla/5.0 ", 100) // 1100 字符
	r.Record(Record{
		Timestamp:  time.Now(),
		Upstream:   "up",
		Model:      "m",
		Endpoint:   "chat",
		Status:     200,
		ClientAddr: "127.0.0.1:5555",
		UserAgent:  longUA,
	})
	select {
	case rec := <-r.ch:
		if len(rec.UserAgent) != 200 {
			t.Errorf("UserAgent length = %d, want 200（截断）", len(rec.UserAgent))
		}
		if rec.UserAgent != longUA[:200] {
			t.Errorf("UserAgent = %q..., want 前 200 字符", rec.UserAgent[:30])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("recorder channel 未收到记录")
	}
}
