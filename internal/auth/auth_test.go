package auth

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/icefairy/xuanji/internal/store"
)

// newTestStore 打开内存 store 并创建测试下游 key。
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// mustCreateToken 创建下游 key，失败即终止。
func mustCreateToken(t *testing.T, s *store.Store, name, key string) {
	t.Helper()
	if _, err := s.CreateAPIToken(name, key, ""); err != nil {
		t.Fatalf("CreateAPIToken(%s): %v", name, err)
	}
}

func TestNew_NoStoreNoJWT_Disabled(t *testing.T) {
	if got := New(nil, ""); got != nil {
		t.Errorf("New(nil, \"\") = %v, want nil（鉴权关闭）", got)
	}
}

func TestNew_JWTOnly_Enabled(t *testing.T) {
	a := New(nil, "jwt-secret")
	if a == nil {
		t.Fatal("New(nil, secret) = nil, want non-nil")
	}
	if !a.Enabled() {
		t.Error("expected enabled")
	}
}

func TestValid_DownstreamKeyFromStore(t *testing.T) {
	s := newTestStore(t)
	mustCreateToken(t, s, "client-a", "sk-test-1")
	mustCreateToken(t, s, "client-b", "sk-test-2")

	a := New(s, "")
	if !a.Enabled() {
		t.Fatal("expected enabled")
	}
	for _, k := range []string{"sk-test-1", "sk-test-2"} {
		if !a.Valid(k) {
			t.Errorf("Valid(%q) = false, want true", k)
		}
	}
	if a.Valid("sk-unknown") {
		t.Error("Valid(sk-unknown) = true, want false")
	}
}

func TestValid_DisabledTokenRejected(t *testing.T) {
	s := newTestStore(t)
	mustCreateToken(t, s, "client", "sk-test-1")

	a := New(s, "")
	if !a.Valid("sk-test-1") {
		t.Fatal("Valid(enabled key) = false, want true")
	}

	// 禁用后 Refresh（管理端流程：SetAPITokenEnabled → refreshAuth）才失效
	id := mustTokenID(t, s, "sk-test-1")
	if err := s.SetAPITokenEnabled(id, false); err != nil {
		t.Fatalf("SetAPITokenEnabled: %v", err)
	}
	a.Refresh()
	if a.Valid("sk-test-1") {
		t.Error("Valid(disabled key) = true, want false")
	}
}

// mustTokenID 按 key 查回 token id。
func mustTokenID(t *testing.T, s *store.Store, key string) uint {
	t.Helper()
	tokens, err := s.ListAPITokens()
	if err != nil {
		t.Fatalf("ListAPITokens: %v", err)
	}
	for _, tk := range tokens {
		if tk.Key == key {
			return tk.ID
		}
	}
	t.Fatalf("token %q not found", key)
	return 0
}

func TestMiddleware_DownstreamKey(t *testing.T) {
	s := newTestStore(t)
	mustCreateToken(t, s, "client", "sk-secret")

	a := New(s, "")
	handler := a.Middleware(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// 无 Authorization
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no auth: got %d, want 401", rec.Code)
	}

	// 错误 key
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong key: got %d, want 401", rec.Code)
	}

	// 正确 key
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-secret")
	rec = httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("valid key: got %d, want 200", rec.Code)
	}

	// x-api-key 头（Anthropic 风格）
	req = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("x-api-key", "sk-secret")
	rec = httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("x-api-key valid: got %d, want 200", rec.Code)
	}

	// x-api-key 错误 key
	req = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("x-api-key", "wrong")
	rec = httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("x-api-key wrong: got %d, want 401", rec.Code)
	}
}

func TestMiddleware_AdminJWT(t *testing.T) {
	s := newTestStore(t)
	a := New(s, "jwt-secret")
	handler := a.Middleware(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	tok := SignToken("jwt-secret", "admin", 3600)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("jwt valid: got %d, want 200", rec.Code)
	}

	// 过期 token 拒绝（-2h 确保 exp 严格小于当前秒）
	expired := SignToken("jwt-secret", "admin", -2*time.Hour)
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+expired)
	rec = httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("jwt expired: got %d, want 401", rec.Code)
	}
}

func TestMiddleware_Disabled(t *testing.T) {
	a := New(nil, "")
	if a != nil {
		t.Fatal("New(nil, \"\") = non-nil, want nil")
	}
	// nil 鉴权器：Middleware 直接透传（开发模式）
	handler := a.Middleware(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("disabled auth: got %d, want 200", rec.Code)
	}
}

func TestRefresh_ReloadsTokens(t *testing.T) {
	s := newTestStore(t)
	a := New(s, "")
	if a.Valid("sk-new") {
		t.Fatal("Valid(sk-new) = true before create, want false")
	}

	mustCreateToken(t, s, "new-client", "sk-new")
	a.Refresh()
	if !a.Valid("sk-new") {
		t.Error("Valid(sk-new) = false after Refresh, want true")
	}
}

func TestName_DownstreamAndJWT(t *testing.T) {
	s := newTestStore(t)
	mustCreateToken(t, s, "client-a", "sk-test-1")

	a := New(s, "jwt-secret")
	if got := a.Name("sk-test-1"); got != "client-a" {
		t.Errorf("Name(sk-test-1) = %q, want client-a", got)
	}

	tok := SignToken("jwt-secret", "admin", 3600)
	if got := a.Name(tok); got != "(admin)" {
		t.Errorf("Name(jwt) = %q, want (admin)", got)
	}

	if got := a.Name("sk-unknown"); got != "sk-unknown" {
		t.Errorf("Name(unknown) = %q, want itself", got)
	}
}
