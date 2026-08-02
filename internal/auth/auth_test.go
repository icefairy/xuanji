package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNew_Empty(t *testing.T) {
	if got := New("", nil, ""); got != nil {
		t.Errorf("New(\"\") = %v, want nil", got)
	}
	if got := New("  , , ", nil, ""); got != nil {
		t.Errorf("New(blank) = %v, want nil", got)
	}
}

func TestNew_Multi(t *testing.T) {
	a := New("k1, k2 ,k3", nil, "")
	if !a.Enabled() {
		t.Error("expected enabled")
	}
	for _, k := range []string{"k1", "k2", "k3"} {
		if !a.Valid(k) {
			t.Errorf("Valid(%q) = false, want true", k)
		}
	}
	if a.Valid("k4") {
		t.Error("Valid(k4) = true, want false")
	}
}

func TestMiddleware_RequiresAuth(t *testing.T) {
	a := New("secret", nil, "")
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
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("valid key: got %d, want 200", rec.Code)
	}

	// x-api-key 头（Anthropic 风格）
	req = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("x-api-key", "secret")
	rec = httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("x-api-key: got %d, want 200", rec.Code)
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

func TestMiddleware_Disabled(t *testing.T) {
	a := New("", nil, "")
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
