// Package auth 提供网关的 API key 鉴权（Bearer 认证）与管理端 JWT。
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/icefairy/xuanji/internal/store"
)

// ===== 管理端 JWT =====

// Claims 是 JWT payload 内容。
type Claims struct {
	Username string `json:"username"`
	Exp      int64  `json:"exp"` // Unix 秒
}

// SignToken 用 HMAC-SHA256 签发 JWT（header.payload.signature）。
func SignToken(secret, username string, ttl time.Duration) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := Claims{Username: username, Exp: time.Now().Add(ttl).Unix()}
	pb, _ := json.Marshal(payload)
	body := header + "." + base64.RawURLEncoding.EncodeToString(pb)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// VerifyToken 验证 JWT，返回用户名。
func VerifyToken(secret, token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("invalid token format")
	}
	// 验证签名
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(parts[2])) {
		return "", errors.New("invalid signature")
	}
	// 解析 payload
	pb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", err
	}
	var c Claims
	if err := json.Unmarshal(pb, &c); err != nil {
		return "", err
	}
	if time.Now().Unix() > c.Exp {
		return "", errors.New("token expired")
	}
	return c.Username, nil
}

// ===== 下游 API Key 鉴权 =====

// APIKeys 是允许访问网关的 key 集合（map 加速查找）。
// 支持三层：静态 key（server.api_keys 配置）+ 动态下游 key（api_tokens 表）+ 管理端 JWT。
type APIKeys struct {
	mu        sync.RWMutex
	keys      map[string]bool   // 静态 key（server.api_keys 配置）
	dbKeys    map[string]bool   // 下游 key 内存缓存（api_tokens 表 enabled=1 的 key，New/Refresh 时加载）
	names     map[string]string // key → 展示名称（api_tokens.name；静态 key 用 key 自身）
	store     *store.Store      // 非 nil 时启用下游 key 动态校验（Refresh 用）
	jwtSecret string            // 非空时管理端 JWT 也可访问 /v1/*（前端测试用）
}

// New 从逗号分隔的 key 列表构建鉴权器。空列表返回 nil（鉴权关闭）。
// store 非 nil 时，除静态 key 外还会校验 api_tokens 表中的下游 key（启动时全量加载到内存缓存）。
// jwtSecret 非空时，管理端登录签发的 JWT 也能通过转发鉴权。
func New(keysCSV string, st *store.Store, jwtSecret string) *APIKeys {
	a := &APIKeys{keys: make(map[string]bool), names: make(map[string]string), store: st, jwtSecret: jwtSecret}
	if strings.TrimSpace(keysCSV) != "" {
		for _, k := range strings.Split(keysCSV, ",") {
			k = strings.TrimSpace(k)
			if k != "" {
				a.keys[k] = true
				a.names[k] = k // 静态 key 名称即 key 本身
			}
		}
	}
	if st != nil {
		a.dbKeys = st.AllEnabledTokenKeys()
		a.names = mergeNames(a.names, st.AllEnabledTokenNames())
	}
	if st == nil && len(a.keys) == 0 && jwtSecret == "" {
		return nil
	}
	return a
}

// mergeNames 合并名称映射：src 优先（后者覆盖前者）。
func mergeNames(dst, src map[string]string) map[string]string {
	if dst == nil {
		dst = make(map[string]string)
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// Refresh 重新从 api_tokens 表加载启用的下游 key 到内存缓存。
// 管理端增删/启停下游 key 后必须调用，否则鉴权用旧缓存。
func (a *APIKeys) Refresh() {
	if a == nil || a.store == nil {
		return
	}
	a.mu.Lock()
	a.dbKeys = a.store.AllEnabledTokenKeys()
	a.names = mergeNames(a.names, a.store.AllEnabledTokenNames())
	a.mu.Unlock()
}

// Name 返回给定 Bearer token 的展示名称（api_tokens.name；静态 key 用 key 自身；
// JWT 返回 "(admin)"；未知返回 key 本身）。
func (a *APIKeys) Name(token string) string {
	if a == nil || token == "" {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if n, ok := a.names[token]; ok {
		return n
	}
	if a.keys[token] {
		return token
	}
	if a.jwtSecret != "" {
		if _, err := VerifyToken(a.jwtSecret, token); err == nil {
			return "(admin)"
		}
	}
	return token
}

// Enabled 表示是否启用鉴权。
func (a *APIKeys) Enabled() bool { return a != nil }

// Valid 判断给定的 Bearer token 是否有效。
// 顺序：静态 key map → 下游 key 内存缓存 → 管理端 JWT。全程无 DB 查询。
func (a *APIKeys) Valid(token string) bool {
	if a == nil {
		return true
	}
	a.mu.RLock()
	ok := a.keys[token]
	if !ok && a.dbKeys != nil {
		ok = a.dbKeys[token]
	}
	a.mu.RUnlock()
	if ok {
		return true
	}
	if a.jwtSecret != "" {
		if _, err := VerifyToken(a.jwtSecret, token); err == nil {
			return true
		}
	}
	return false
}

// Middleware 校验 API key，支持两种携带方式：
//
//	Authorization: Bearer *** 风格）
//	x-api-key: *** 风格）
//
// 未配置鉴权时直接放行；未带或无效 key 返回 401。
func (a *APIKeys) Middleware(next http.HandlerFunc) http.HandlerFunc {
	if a == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		token := ""
		h := r.Header.Get("Authorization")
		if strings.HasPrefix(h, "Bearer ") {
			token = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		} else if xk := r.Header.Get("x-api-key"); xk != "" {
			token = strings.TrimSpace(xk)
		}
		if !a.Valid(token) {
			writeUnauthorized(w)
			return
		}
		next(w, r)
	}
}

// writeUnauthorized 以 OpenAI 标准错误格式写 401。
func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"message": "Invalid API key",
			"type":    "invalid_request_error",
			"code":    "invalid_api_key",
		},
	})
}
