package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/icefairy/xuanji/internal/store"
)

// newBatchTestHandler 构建带 store 与 reload 计数的管理 Handler。
func newBatchTestHandler(t *testing.T) (*Handler, *store.Store, *int) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	h, hc := newTestHandler(t, testConfig())
	t.Cleanup(hc.Close)
	h.SetStore(s)
	reloaded := 0
	h.SetReload(func() error { reloaded++; return nil })
	return h, s, &reloaded
}

// TestBatchDeleteUpstreamsHandler 验证 POST /admin/upstreams/batch-delete：
// 一次删多个上游、级联清理孤儿配置键、只热重载一次；空列表与不存在的名字不报错。
func TestBatchDeleteUpstreamsHandler(t *testing.T) {
	h, s, reloaded := newBatchTestHandler(t)
	for _, name := range []string{"up-a", "up-b", "up-c"} {
		if err := s.CreateUpstream(&store.UpstreamRow{
			Name: name, Type: "openai", BaseURL: "http://x/" + name, Tier: "free",
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetConfig("upstream."+name+".per_model_billing", "1"); err != nil {
			t.Fatal(err)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/upstreams/batch-delete", h.BatchDeleteUpstreams)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/upstreams/batch-delete",
		strings.NewReader(`{"names":["up-a","up-b","no-such"]}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	decodeBody(t, rr, &out)
	if out["deleted"].(float64) != 2 {
		t.Errorf("deleted = %v, want 2", out["deleted"])
	}
	if *reloaded != 1 {
		t.Errorf("reload called %d times, want 1（批量删除只重载一次）", *reloaded)
	}
	rows, err := s.ListUpstreams()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "up-c" {
		t.Errorf("remaining = %+v, want [up-c]", rows)
	}
	for _, k := range []string{"upstream.up-a.per_model_billing", "upstream.up-b.per_model_billing"} {
		if v, _ := s.GetConfig(k); v != "" {
			t.Errorf("%s 应随上游删除被清理，got %q", k, v)
		}
	}
	if v, _ := s.GetConfig("upstream.up-c.per_model_billing"); v != "1" {
		t.Errorf("保留上游的配置键被误删，got %q", v)
	}

	// 空 names：400，且不触发重载
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/admin/upstreams/batch-delete",
		strings.NewReader(`{"names":[]}`)))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("empty names: status = %d, want 400", rr.Code)
	}
	if *reloaded != 1 {
		t.Errorf("空列表不应触发重载，reload called %d times", *reloaded)
	}
}

// TestBatchDeleteConfigHandler 验证 DELETE /admin/config（批量）：
// 孤儿键成批删除；含系统关键键时整体拒绝且一条都不删。
func TestBatchDeleteConfigHandler(t *testing.T) {
	h, s, reloaded := newBatchTestHandler(t)
	for _, k := range []string{
		"upstream.ghost.per_model_billing", "upstream.ghost.vendor", "alert.webhook_url",
	} {
		if err := s.SetConfig(k, "1"); err != nil {
			t.Fatal(err)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /admin/config", h.BatchDeleteConfig)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/admin/config",
		strings.NewReader(`{"names":["upstream.ghost.per_model_billing","upstream.ghost.vendor"]}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	decodeBody(t, rr, &out)
	if out["deleted"].(float64) != 2 {
		t.Errorf("deleted = %v, want 2", out["deleted"])
	}
	if *reloaded != 1 {
		t.Errorf("reload called %d times, want 1", *reloaded)
	}

	// 含系统关键键：整体拒绝，且被列为删除的普通键也不能删（避免删一半）
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/admin/config",
		strings.NewReader(`{"names":["alert.webhook_url","server.port"]}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("含 server.port: status = %d, want 400", rr.Code)
	}
	if v, _ := s.GetConfig("alert.webhook_url"); v != "1" {
		t.Errorf("整体拒绝时不应删除任何键，alert.webhook_url = %q", v)
	}
}

// TestOrphanConfigsHandler 验证 GET /admin/config/orphans 只报上游已不存在的键。
func TestOrphanConfigsHandler(t *testing.T) {
	h, s, _ := newBatchTestHandler(t)
	if err := s.CreateUpstream(&store.UpstreamRow{
		Name: "alive", Type: "openai", BaseURL: "http://x", Tier: "free",
	}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"upstream.alive.per_model_billing", "upstream.ghost.vendor"} {
		if err := s.SetConfig(k, "1"); err != nil {
			t.Fatal(err)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/config/orphans", h.OrphanConfigs)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/config/orphans", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Upstreams []string `json:"upstreams"`
		Keys      []string `json:"keys"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Upstreams) != 1 || out.Upstreams[0] != "ghost" {
		t.Errorf("upstreams = %v, want [ghost]", out.Upstreams)
	}
	if len(out.Keys) != 1 || out.Keys[0] != "upstream.ghost.vendor" {
		t.Errorf("keys = %v, want [upstream.ghost.vendor]", out.Keys)
	}
}
