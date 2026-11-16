package quota

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/icefairy/xuanji/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func rec(s *store.Store, name, model string, when time.Time, tokens int64) {
	if err := s.Insert(store.Record{
		Timestamp: when,
		Upstream:  "up",
		Model:     model,
		Endpoint:  "chat",
		Status:    200,
		Tokens:    tokens,
		APIKey:    name,
	}); err != nil {
		panic(err)
	}
}

// setupGroup1User 建一个组 g1（白名单 ds/qwen），ds 与 glm 各一档配额，给 alice 入组。
func setupGroup1User(t *testing.T, st *store.Store) (aliceKey string, gid uint) {
	g, err := st.CreateGroup("dev", `["ds-v4-flash","qwen-max"]`, "")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	gid = g.ID
	if err := st.UpsertGroupQuota(gid, "ds-v4-flash", 1000, 5000, 20000); err != nil {
		t.Fatalf("UpsertGroupQuota ds: %v", err)
	}
	if err := st.UpsertGroupQuota(gid, "glm-5.2", 200, 500, 1000); err != nil {
		t.Fatalf("UpsertGroupQuota glm: %v", err)
	}
	// 默认档（白名单外的模型不可用；不限模型兜底不配）
	tok, err := st.CreateAPIToken("alice", "sk-alice", "")
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	if err := st.UpdateAPITokenPolicy(tok.ID, gid, "", ""); err != nil {
		t.Fatalf("UpdateAPITokenPolicy: %v", err)
	}
	return tok.Key, gid
}

func mustSvc(t *testing.T, st *store.Store) *Service {
	t.Helper()
	s := New(st)
	s.Load()
	if !s.Enabled() {
		t.Fatalf("service not enabled")
	}
	return s
}

// newSvcWithClock 构建带可注入时钟的服务（窗口滑动/跨周跨月可测）。
func newSvcWithClock(t *testing.T, st *store.Store, fixed time.Time) *Service {
	t.Helper()
	s := New(st)
	s.Load()
	s.SetClock(func() time.Time { return fixed })
	return s
}

func TestCheck_ModelIndependentPools(t *testing.T) {
	st := openTestStore(t)
	alice, _ := setupGroup1User(t, st)
	svc := mustSvc(t, st)

	// alice 用 ds-v4-flash 累计 900（组 ds 5h=1000）
	svc.AddUsage("alice", "ds-v4-flash", 900)
	if qe := svc.Check(alice, "ds-v4-flash"); qe != nil {
		t.Fatalf("900<1000 should pass, got %v", qe)
	}
	// 再 200 → 1100 >= 1000 → 429
	svc.AddUsage("alice", "ds-v4-flash", 200)
	qe := svc.Check(alice, "ds-v4-flash")
	if qe == nil || qe.Status != 429 || qe.Code != "quota_exceeded" {
		t.Fatalf("expect 429 quota_exceeded, got %+v", qe)
	}
	// glm 池独立：配了额度隐含允许，还没用 → 照常可用
	if qe := svc.Check(alice, "glm-5.2"); qe != nil {
		t.Fatalf("glm pool untouched should pass, got %v", qe)
	}
	// glm 用 200 → 超 200 上限（>=）
	svc.AddUsage("alice", "glm-5.2", 200)
	qe = svc.Check(alice, "glm-5.2")
	if qe == nil || qe.Status != 429 {
		t.Fatalf("glm 200>=200 should 429, got %+v", qe)
	}
	// ds 已超但 glm 超不影响其结论
	if qe := svc.Check(alice, "ds-v4-flash"); qe == nil || qe.Code != "quota_exceeded" {
		t.Fatalf("ds still exceeded, got %+v", qe)
	}
}

func TestCheck_Whitelist403(t *testing.T) {
	st := openTestStore(t)
	alice, _ := setupGroup1User(t, st)
	svc := mustSvc(t, st)

	// xyz-9：无配额 + 不在组白名单 [ds-v4-flash, qwen-max] → 403
	qe := svc.Check(alice, "xyz-9")
	if qe == nil || qe.Status != 403 || qe.Code != "model_not_allowed" {
		t.Fatalf("expect 403 model_not_allowed, got %+v", qe)
	}
	// glm-5.2：有配额 → 隐含允许（不再依赖白名单）
	if qe := svc.Check(alice, "glm-5.2"); qe != nil {
		t.Fatalf("glm has quota row → implicitly allowed, got %v", qe)
	}
	// qwen-max：白名单内且无配额 → 放行不限量
	if qe := svc.Check(alice, "qwen-max"); qe != nil {
		t.Fatalf("qwen-max in whitelist, no quota row → pass, got %v", qe)
	}
}

func TestCheck_KeyOverrideWins(t *testing.T) {
	st := openTestStore(t)
	_, gid := setupGroup1User(t, st)
	svc := mustSvc(t, st)

	// alice 无 override：ds 5h 组 1000
	// bob 入组但 override ds 5h=50
	tok, _ := st.CreateAPIToken("bob", "sk-bob", "")
	if err := st.UpdateAPITokenPolicy(tok.ID, gid, "", `{"ds-v4-flash":{"5h":50,"week":100,"month":300}}`); err != nil {
		t.Fatalf("UpdateAPITokenPolicy bob: %v", err)
	}
	svc.Refresh()

	svc.AddUsage("bob", "ds-v4-flash", 60)
	if qe := svc.Check(tok.Key, "ds-v4-flash"); qe == nil || qe.Status != 429 {
		t.Fatalf("bob 60>=50 should 429 via key override, got %+v", qe)
	}
	// whitelist 继承组：qwen-max 也放行
	if qe := svc.Check(tok.Key, "qwen-max"); qe != nil {
		t.Fatalf("bob qwen-max should pass (inherit group whitelist), got %v", qe)
	}
}

func TestCheck_WeekMonthWindows(t *testing.T) {
	st := openTestStore(t)
	g, err := st.CreateGroup("wm", `[]`, "")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	// 两个独立模型：slow 只限周(5h/月巨大)，month-driven 只限月(5h/周巨大)
	if err := st.UpsertGroupQuota(g.ID, "slow-model", 1000000000, 5000, 1000000000); err != nil {
		t.Fatalf("quota slow: %v", err)
	}
	if err := st.UpsertGroupQuota(g.ID, "month-driven", 1000000000, 1000000000, 5000); err != nil {
		t.Fatalf("quota month-driven: %v", err)
	}
	tok, _ := st.CreateAPIToken("cw", "sk-cw", "")
	if err := st.UpdateAPITokenPolicy(tok.ID, g.ID, "", ""); err != nil {
		t.Fatalf("policy: %v", err)
	}
	// 固定时钟：2026-08-20 15:00 UTC（周四，周起点 08-17，月起点 08-01）
	fixed := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	svc := newSvcWithClock(t, st, fixed)

	// 周：slow 本周 6000 > 5000 → 周超
	svc.AddUsage("cw", "slow-model", 6000)
	qe := svc.Check(tok.Key, "slow-model")
	if qe == nil || qe.Code != "quota_exceeded" {
		t.Fatalf("week exceeded should 429, got %+v", qe)
	}
	// 月：month-driven 本月 6000 > 5000 → 月超
	svc.AddUsage("cw", "month-driven", 6000)
	qe = svc.Check(tok.Key, "month-driven")
	if qe == nil || qe.Code != "quota_exceeded" {
		t.Fatalf("month exceeded should 429, got %+v", qe)
	} else if qe.Details["exceeded"] != "month" {
		t.Fatalf("exceeded should be month, got %v", qe.Details["exceeded"])
	}

	// 跨周：跳到下周一 08-24 → week 重置；5h 窗口旧记录出窗；week 仅剩新量
	svc.SetClock(func() time.Time { return time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC) })
	svc.AddUsage("cw", "slow-model", 100)
	qe = svc.Check(tok.Key, "slow-model")
	if qe != nil {
		t.Fatalf("after week reset slow 100<5000 should pass, got %+v", qe)
	}
	// 跨月：跳到 09-01 → month 重置
	svc.SetClock(func() time.Time { return time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC) })
	svc.AddUsage("cw", "month-driven", 100)
	if qe := svc.Check(tok.Key, "month-driven"); qe != nil {
		t.Fatalf("after month reset month-driven 100<5000 should pass, got %+v", qe)
	}
}

func TestMiddleware_Quota429AndBodyPreserved(t *testing.T) {
	st := openTestStore(t)
	alice, _ := setupGroup1User(t, st)
	svc := New(st)
	svc.Load()

	// 先让 alice 的 ds 5h 超限（1000）
	svc.AddUsage("alice", "ds-v4-flash", 1000)

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		_ = json.Unmarshal(body, &m)
		w.Header().Set("X-Next-Model", m["model"].(string))
		w.WriteHeader(200)
	})
	h := svc.Middleware(next)

	// 超限 → 429
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{"model":"ds-v4-flash","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+alice)
	recw := httptest.NewRecorder()
	h(recw, req)
	if recw.Code != 429 {
		t.Fatalf("expect 429, got %d body=%s", recw.Code, recw.Body.String())
	}
	if !strings.Contains(recw.Body.String(), "quota_exceeded") {
		t.Fatalf("body should contain quota_exceeded: %s", recw.Body.String())
	}

	// 未超限 → 放行且 body 原样保留
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"qwen-max","messages":[]}`))
	req2.Header.Set("Authorization", "Bearer "+alice)
	h(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("qwen-max untouched should pass, got %d", rec2.Code)
	}
	if got := rec2.Header().Get("X-Next-Model"); got != "qwen-max" {
		t.Fatalf("model not preserved in body, got %q", got)
	}
}

func TestMiddleware_NoPolicyPasses(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.CreateAPIToken("free", "sk-free", ""); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	svc := New(st)
	svc.Load()

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	h := svc.Middleware(next)
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(`{"model":"any-model"}`))
	req.Header.Set("Authorization", "Bearer sk-free")
	recw := httptest.NewRecorder()
	h(recw, req)
	if recw.Code != 200 {
		t.Fatalf("free key with no policy should pass, got %d", recw.Code)
	}
}

// TestWeekMonthStart 校验窗口起算边界。
func TestWeekMonthStart(t *testing.T) {
	mon := WeekStart(time.Date(2026, 8, 20, 15, 30, 0, 0, time.UTC)) // 2026-08-20 是周四
	if mon.Weekday() != time.Monday || mon.Hour() != 0 || mon.Minute() != 0 {
		t.Fatalf("WeekStart got %v", mon)
	}
	if got := WeekStart(time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)); !got.Equal(time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("WeekStart monday edge got %v", got)
	}
	ms := MonthStart(time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC))
	if ms.Month() != time.August || ms.Day() != 1 || ms.Hour() != 0 {
		t.Fatalf("MonthStart got %v", ms)
	}
}
