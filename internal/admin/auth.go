package admin

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/icefairy/xuanji/internal/auth"
	"golang.org/x/crypto/bcrypt"
)

// Login 管理端用户名密码登录（POST /admin/login）。
// 验证 bcrypt 密码后签发 JWT，有效期 24h。
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	if req.Username == "" || req.Password == "" {
		writeJSON(w, map[string]string{"error": "username and password required"})
		return
	}
	if h.store == nil {
		writeJSON(w, map[string]string{"error": "store not available"})
		return
	}
	user, err := h.store.GetUser(req.Username)
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if user == nil || bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)) != nil {
		writeJSON(w, map[string]string{"error": "invalid username or password"})
		return
	}
	secret := h.JWTSecret()
	token := auth.SignToken(secret, user.Username, 24*time.Hour)
	writeJSON(w, map[string]string{"status": "ok", "token": token, "username": user.Username})
}

// ChangePassword 修改当前用户密码（PUT /admin/password）。
// 需携带旧密码 + 新密码，验证通过后更新。
func (h *Handler) ChangePassword(w http.ResponseWriter, r *http.Request) {
	username, _ := r.Context().Value("xuanji-username").(string)
	if username == "" {
		writeJSON(w, map[string]string{"error": "unauthorized"})
		return
	}
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	if req.NewPassword == "" || len(req.NewPassword) < 6 {
		writeJSON(w, map[string]string{"error": "新密码至少 6 位"})
		return
	}
	if h.store == nil {
		writeJSON(w, map[string]string{"error": "store not available"})
		return
	}
	user, err := h.store.GetUser(username)
	if err != nil || user == nil {
		writeJSON(w, map[string]string{"error": "user not found"})
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.OldPassword)) != nil {
		writeJSON(w, map[string]string{"error": "旧密码错误"})
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if err := h.store.UpdateUserPassword(username, string(hash)); err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// JWTSecret 读取或生成 JWT 签名密钥（config 表 admin.jwt_secret）。
func (h *Handler) JWTSecret() string {
	if h.store == nil {
		return generateToken()
	}
	v, _ := h.store.GetConfig("admin.jwt_secret")
	if v != "" {
		return v
	}
	v = "xuanji-" + generateToken()
	_ = h.store.SetConfig("admin.jwt_secret", v)
	return v
}
