// Package main 是璇玑网关的服务入口：加载配置、启动健康检查与 HTTP 服务。
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/icefairy/xuanji/internal/admin"
	"github.com/icefairy/xuanji/internal/anthropic"
	"github.com/icefairy/xuanji/internal/auth"
	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/gemini"
	"github.com/icefairy/xuanji/internal/health"
	"github.com/icefairy/xuanji/internal/ollama"
	"github.com/icefairy/xuanji/internal/proxy"
	"github.com/icefairy/xuanji/internal/quota"
	"github.com/icefairy/xuanji/internal/router"
	"github.com/icefairy/xuanji/internal/store"
)

//go:embed all:web
var embeddedWeb embed.FS

// webFileSystem 返回管理界面静态文件系统根（含 admin_vue.html 与 vue/ 子目录）：
// 优先磁盘 web/ 目录（开发时可改模板即时生效），不存在时回退到嵌入的 web
// （单二进制发布/Docker 场景，保证管理页可用）。
func webFileSystem() http.FileSystem {
	if _, err := os.Stat("web"); err == nil {
		return http.Dir("web")
	}
	sub, err := fs.Sub(embeddedWeb, "web")
	if err != nil {
		return http.FS(embeddedWeb)
	}
	return http.FS(sub)
}

// webVueFileSystem 返回 vue 静态资源子目录（/vue/ 路由专用）。
func webVueFileSystem() http.FileSystem {
	if _, err := os.Stat("web/vue"); err == nil {
		return http.Dir("web/vue")
	}
	sub, err := fs.Sub(embeddedWeb, "web/vue")
	if err != nil {
		return http.FS(embeddedWeb)
	}
	return http.FS(sub)
}

// writeJSONError 以 JSON 格式写错误响应。
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// probeRecorder 构造探测结果持久化器：健康检查每次探测后落库，
// 供 metrics 按时间范围聚合健康度；重启后历史保留，健康度不归零。
func probeRecorder(st *store.Store) health.ProbeRecorder {
	if st == nil {
		return nil
	}
	return probeStoreRecorder{st: st}
}

// probeStoreRecorder 是 health.ProbeRecorder 的 store 实现。
type probeStoreRecorder struct {
	st *store.Store
}

func (r probeStoreRecorder) RecordProbe(upstream string, ok bool, at time.Time) {
	if err := r.st.RecordProbe(upstream, ok, at); err != nil {
		slog.Debug("record probe failed", "upstream", upstream, "ok", ok, "error", err)
	}
}

// admJWTSecret 从 config 表读取/生成 JWT 签名密钥（与 admin.Handler 共用）。
func admJWTSecret(st *store.Store) string {
	if st == nil {
		return ""
	}
	v, _ := st.GetConfig("admin.jwt_secret")
	if v != "" {
		return v
	}
	// 生成随机 secret 并持久化（首次启动）
	b := make([]byte, 32)
	if _, err := rand.Read(b); err == nil {
		v = "xuanji-" + hex.EncodeToString(b)
	} else {
		v = fmt.Sprintf("xuanji-%d", time.Now().UnixNano())
	}
	_ = st.SetConfig("admin.jwt_secret", v)
	return v
}

// appState 持有运行时可热替换的组件。
type appState struct {
	mu  sync.RWMutex
	hc  *health.Checker
	srv *http.Server
	qm  *quota.Service // 配额策略（组×模型 白名单+模型级配额）
}

var (
	cfg   *config.Config
	state appState

	flagPort = flag.Int("port", 8787, "监听端口（默认 8787）")
	flagDB   = flag.String("db", "/data/codes/xuanji/data/xuanji.db", "数据库路径（默认 /data/codes/xuanji/data/xuanji.db）")
	flagHelp = flag.Bool("help", false, "显示帮助")
)

func main() {
	flag.Parse()
	if *flagHelp {
		flag.Usage()
		os.Exit(0)
	}

	// 1. 打开数据库（必须，所有配置存于此）
	storeInst, err := store.Open(*flagDB)
	if err != nil {
		slog.Error("open store", "path", *flagDB, "error", err)
		os.Exit(1)
	}
	rec := store.NewRecorder(storeInst)
	defer func() {
		slog.Info("closing store")
		rec.Close()
		storeInst.Close()
	}()
	slog.Info("store opened", "path", *flagDB)

	// 2. 写入默认配置（仅首次启动）
	if err := storeInst.SeedDefaults(); err != nil {
		slog.Error("seed defaults", "error", err)
		os.Exit(1)
	}

	// 2.2 确保管理 API key 存在（首次生成随机 key，系统设置页可见可改）
	if err := ensureAdminAPIKey(storeInst); err != nil {
		slog.Error("ensure admin api key", "error", err)
		os.Exit(1)
	}

	// 2.5 首次启动创建默认管理用户 admin / xuanji123
	if n, _ := storeInst.CountUsers(); n == 0 {
		hash, _ := bcrypt.GenerateFromPassword([]byte("xuanji123"), bcrypt.DefaultCost)
		if _, err := storeInst.CreateUser("admin", string(hash)); err != nil {
			slog.Error("create default user", "error", err)
		} else {
			slog.Info("created default admin user (admin / xuanji123)")
		}
	}

	// 3. 从 DB 加载全部配置
	cfg, err = config.LoadFromDB(storeInst)
	if err != nil {
		slog.Error("load config from db", "error", err)
		os.Exit(1)
	}

	// 4. CLI 端口覆盖数据库配置
	cfg.Server.Port = *flagPort

	slog.Info("config loaded from database", "upstreams", len(cfg.Upstreams), "rules", len(cfg.Routing.Rules))

	rt := router.New(cfg)
	hc := health.New(cfg)
	hc.SetProbeRecorder(probeRecorder(storeInst))
	hc.Start()

	state.mu.Lock()
	state.hc = hc
	state.mu.Unlock()

	// API key 鉴权（server.api_keys 配置后生效，/healthz 不鉴权）
	// storeInst 非 nil 时，api_tokens 表中的下游 key 也参与转发鉴权；
	// 管理端 JWT 也放行（前端测试按钮直接用登录 token 调转发）
	keys := auth.New(storeInst, admJWTSecret(storeInst))
	// 配额策略（组×模型 白名单 + 模型级配额；启动即加载，管理端改动后 Refresh）
	qm := quota.New(storeInst)
	state.mu.Lock()
	state.qm = qm
	state.mu.Unlock()
	mux := buildServeMux(cfg, rt, hc, rec, storeInst, keys, qm)

	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	state.mu.Lock()
	state.srv = srv
	state.mu.Unlock()

	// 优雅关闭：SIGINT/SIGTERM 时停止健康检查并退出 HTTP 服务。
	go func() {
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		<-quit
		slog.Info("shutting down")
		hc.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	// 每周自动备份：启动后立即做一次（确保服务重启也有备份），之后每 7 天一次。
	// 备份为 gzip 压缩快照，自动保留最近 10 个（store.CreateBackup 由 admin 层轮转）。
	if storeInst != nil {
		go func() {
			backupOnce(storeInst)
			ticker := time.NewTicker(7 * 24 * time.Hour)
			defer ticker.Stop()
			for range ticker.C {
				backupOnce(storeInst)
			}
		}()
	}

	slog.Info("xuanji gateway listening",
		"addr", addr,
		"upstreams", len(cfg.Upstreams),
		"default_strategy", cfg.Routing.DefaultStrategy,
	)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
	hc.Close()
}

// backupOnce 执行一次自动备份并轮转保留最近 10 个，失败只记日志不影响主流程。
func backupOnce(storeInst *store.Store) {
	name, err := storeInst.CreateBackup()
	if err != nil {
		slog.Error("auto backup failed", "error", err)
		return
	}
	// 轮转保留最近 10 个
	entries, _ := os.ReadDir(storeInst.BackupDir())
	var gz []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".gz") {
			gz = append(gz, e.Name())
		}
	}
	removed := 0
	for len(gz) > 10 {
		sort.Strings(gz)
		old := gz[0]
		if os.Remove(filepath.Join(storeInst.BackupDir(), old)) == nil {
			removed++
		}
		gz = gz[1:]
	}
	slog.Info("auto backup done", "name", name, "pruned", removed)
}

// buildServeMux 构造 HTTP 路由 mux。
// qm 为配额策略服务（nil 时不启用配额检查）。
func buildServeMux(cfg *config.Config, rt *router.Router, hc *health.Checker, rec *store.Recorder, storeInst *store.Store, apiKeys *auth.APIKeys, qm *quota.Service) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthzHandler)

	// 管理界面（Vue 页面，/admin/* 留给 JSON API）
	// 静态资源从 webFileSystem 读取：磁盘 web/ 目录优先（开发），否则用 go:embed 嵌入资源（单二进制/Docker）。
	webFS := webFileSystem()
	mux.Handle("GET /vue/", http.StripPrefix("/vue/", http.FileServer(webVueFileSystem())))
	mux.Handle("GET /web/", http.StripPrefix("/web/", http.FileServer(webFS)))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		f, err := webFS.Open("admin_vue.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		http.ServeContent(w, r, "admin_vue.html", time.Time{}, f)
	})

	// 管理接口（admin API，加认证：检查 Authorization header 是否匹配 server.api_keys）
	admHandler := admin.New(cfg, hc)
	admHandler.SetAuth(apiKeys)
	if qm != nil {
		admHandler.SetQuotaReload(qm.Refresh)
	}
	if rec != nil {
		admHandler.SetStore(rec.Store())
	}
	if storeInst != nil {
		admHandler.SetReload(func() error {
			return reloadConfig(storeInst, rec)
		})
	}
	adminAuth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			// 管理端统一走 JWT（用户名密码登录），不再用 API Key
			authHdr := r.Header.Get("Authorization")
			token := strings.TrimPrefix(authHdr, "Bearer ")
			if token == "" || token == authHdr {
				writeJSONError(w, http.StatusUnauthorized, "未登录")
				return
			}
			username, err := auth.VerifyToken(admHandler.JWTSecret(), token)
			if err != nil {
				writeJSONError(w, http.StatusUnauthorized, "登录已过期，请重新登录")
				return
			}
			// 将用户名写入请求上下文，供 ChangePassword 使用
			ctx := context.WithValue(r.Context(), "xuanji-username", username)
			next(w, r.WithContext(ctx))
		}
	}
	// 登录接口不鉴权
	mux.HandleFunc("POST /admin/login", admHandler.Login)

	// 管理 API 中间件：独立 key（config 表 admin.api_key）鉴权，供 AI 助手免登录调用
	adminKeyAuth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if cfg.Server.AdminAPIKey == "" {
				writeJSONError(w, http.StatusUnauthorized, "管理 API key 未配置")
				return
			}
			authHdr := r.Header.Get("Authorization")
			token := strings.TrimPrefix(authHdr, "Bearer ")
			if token == "" || token == authHdr || token != cfg.Server.AdminAPIKey {
				writeJSONError(w, http.StatusUnauthorized, "无效的管理 API key")
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("PUT /admin/password", adminAuth(admHandler.ChangePassword))
	mux.HandleFunc("GET /admin/status", adminAuth(admHandler.Status))
	mux.HandleFunc("GET /admin/upstreams", adminAuth(admHandler.Upstreams))
	mux.HandleFunc("GET /admin/rules", adminAuth(admHandler.Rules))
	mux.HandleFunc("GET /admin/config", adminAuth(admHandler.GetAllConfig))
	mux.HandleFunc("PUT /admin/config", adminAuth(admHandler.UpdateConfig))
	mux.HandleFunc("GET /admin/metrics/summary", adminAuth(admHandler.MetricsSummary))
	mux.HandleFunc("GET /admin/metrics/upstreams", adminAuth(admHandler.MetricsUpstreams))
	mux.HandleFunc("GET /admin/metrics/hourly", adminAuth(admHandler.MetricsHourly))
	mux.HandleFunc("GET /admin/metrics/daily", adminAuth(admHandler.MetricsDaily))
	mux.HandleFunc("GET /admin/metrics/keys", adminAuth(admHandler.MetricsByAPIKey))
	mux.HandleFunc("GET /admin/metrics/keys/{name}/models", adminAuth(admHandler.MetricsByAPIKeyModels))
	mux.HandleFunc("GET /admin/config/retry", adminAuth(admHandler.GetRetryConfig))
	mux.HandleFunc("GET /admin/logs", adminAuth(admHandler.RequestLogs))
	mux.HandleFunc("POST /admin/logs/recalc-cost", adminAuth(admHandler.RecalcCost))
	mux.HandleFunc("GET /admin/api-keys", adminAuth(admHandler.APIKeys))
	mux.HandleFunc("POST /admin/api-keys", adminAuth(admHandler.AddAPIKey))
	mux.HandleFunc("DELETE /admin/api-keys/{key}", adminAuth(admHandler.DeleteAPIKey))
	mux.HandleFunc("PUT /admin/api-keys/{id}/toggle", adminAuth(admHandler.SetAPIKeyEnabled))
	mux.HandleFunc("PUT /admin/api-keys/{id}/name", adminAuth(admHandler.RenameAPIKey))
	mux.HandleFunc("PUT /admin/api-keys/{id}/policy", adminAuth(admHandler.UpdateAPIKeyPolicy))

	// 分组管理（配额矩阵）：组∖模型 独立配额，组内每人默认，key 可覆盖
	mux.HandleFunc("GET /admin/groups", adminAuth(admHandler.Groups))
	mux.HandleFunc("POST /admin/groups", adminAuth(admHandler.CreateGroup))
	mux.HandleFunc("PUT /admin/groups/{id}", adminAuth(admHandler.UpdateGroup))
	mux.HandleFunc("DELETE /admin/groups/{id}", adminAuth(admHandler.DeleteGroup))
	mux.HandleFunc("PUT /admin/groups/{id}/quotas", adminAuth(admHandler.UpdateGroupQuota))
	mux.HandleFunc("DELETE /admin/groups/{id}/quotas/{model}", adminAuth(admHandler.DeleteGroupQuota))

	// CRUD 端点
	mux.HandleFunc("POST /admin/upstreams", adminAuth(admHandler.CreateUpstream))
	mux.HandleFunc("PUT /admin/upstreams/{name}", adminAuth(admHandler.UpdateUpstream))
	mux.HandleFunc("DELETE /admin/upstreams/{name}", adminAuth(admHandler.DeleteUpstream))
	mux.HandleFunc("POST /admin/upstreams/{name}/clone", adminAuth(admHandler.CloneUpstream))
	mux.HandleFunc("POST /admin/upstreams/{name}/test", adminAuth(admHandler.TestUpstream))
	mux.HandleFunc("GET /admin/upstreams/{name}/models", adminAuth(admHandler.UpstreamModels))
	mux.HandleFunc("POST /admin/rules", adminAuth(admHandler.CreateRoutingRule))
	mux.HandleFunc("PUT /admin/rules/{model}", adminAuth(admHandler.UpdateRoutingRule))
	mux.HandleFunc("DELETE /admin/rules/{model}", adminAuth(admHandler.DeleteRoutingRule))
	mux.HandleFunc("GET /admin/efforts", adminAuth(admHandler.EffortConfigs))
	mux.HandleFunc("POST /admin/efforts", adminAuth(admHandler.CreateEffortConfig))
	mux.HandleFunc("PUT /admin/efforts/{model}", adminAuth(admHandler.UpdateEffortConfig))
	mux.HandleFunc("DELETE /admin/efforts/{model}", adminAuth(admHandler.DeleteEffortConfig))
	mux.HandleFunc("POST /admin/reload", adminAuth(admHandler.Reload))
	mux.HandleFunc("PUT /admin/upstreams/{name}/toggle", adminAuth(admHandler.ToggleUpstream))

	// 管理 API（/api/admin/*）：AI 助手用 admin.api_key 免登录调用，动态修改配置立即生效
	mux.HandleFunc("GET /api/admin/status", adminKeyAuth(admHandler.Status))
	mux.HandleFunc("GET /api/admin/upstreams", adminKeyAuth(admHandler.Upstreams))
	mux.HandleFunc("POST /api/admin/upstreams", adminKeyAuth(admHandler.CreateUpstream))
	mux.HandleFunc("PUT /api/admin/upstreams/{name}", adminKeyAuth(admHandler.UpdateUpstream))
	mux.HandleFunc("PUT /api/admin/upstreams/{name}/toggle", adminKeyAuth(admHandler.ToggleUpstream))
	mux.HandleFunc("DELETE /api/admin/upstreams/{name}", adminKeyAuth(admHandler.DeleteUpstream))
	mux.HandleFunc("POST /api/admin/upstreams/{name}/clone", adminKeyAuth(admHandler.CloneUpstream))
	mux.HandleFunc("POST /api/admin/upstreams/{name}/test", adminKeyAuth(admHandler.TestUpstream))
	mux.HandleFunc("GET /api/admin/upstreams/{name}/models", adminKeyAuth(admHandler.UpstreamModels))
	mux.HandleFunc("GET /api/admin/rules", adminKeyAuth(admHandler.Rules))
	mux.HandleFunc("POST /api/admin/rules", adminKeyAuth(admHandler.CreateRoutingRule))
	mux.HandleFunc("PUT /api/admin/rules/{model}", adminKeyAuth(admHandler.UpdateRoutingRule))
	mux.HandleFunc("DELETE /api/admin/rules/{model}", adminKeyAuth(admHandler.DeleteRoutingRule))
	mux.HandleFunc("GET /api/admin/config", adminKeyAuth(admHandler.GetAllConfig))
	mux.HandleFunc("PUT /api/admin/config", adminKeyAuth(admHandler.UpdateConfig))
	mux.HandleFunc("GET /api/admin/logs", adminKeyAuth(admHandler.RequestLogs))
	mux.HandleFunc("POST /api/admin/reload", adminKeyAuth(admHandler.Reload))

	// 渠道优惠时段
	mux.HandleFunc("GET /admin/discounts", adminAuth(admHandler.Discounts))
	mux.HandleFunc("POST /admin/discounts", adminAuth(admHandler.AddDiscount))
	mux.HandleFunc("PUT /admin/discounts/{id}", adminAuth(admHandler.UpdateDiscount))
	mux.HandleFunc("DELETE /admin/discounts/{id}", adminAuth(admHandler.DeleteDiscount))

	// 模型单价（计费）
	mux.HandleFunc("GET /admin/prices", adminAuth(admHandler.Prices))
	mux.HandleFunc("POST /admin/prices", adminAuth(admHandler.AddPrice))
	mux.HandleFunc("PUT /admin/prices/{model}", adminAuth(admHandler.UpdatePrice))
	mux.HandleFunc("DELETE /admin/prices/{model}", adminAuth(admHandler.DeletePrice))
	// 费用统计（总金额/上游/apikey/模型 四维）
	mux.HandleFunc("GET /admin/metrics/cost", adminAuth(admHandler.MetricsCost))

	// 数据库备份（手动 + 列表 + 删除）
	mux.HandleFunc("GET /admin/backups", adminAuth(admHandler.Backups))
	mux.HandleFunc("POST /admin/backups", adminAuth(admHandler.CreateBackup))
	mux.HandleFunc("DELETE /admin/backups/{name}", adminAuth(admHandler.DeleteBackup))

	// 对话调试：走完整网关路由链路（依赖 pxHandler，见上方 SetProxy 注入）
	mux.HandleFunc("POST /admin/chat", adminAuth(admHandler.Chat))

	olHandler := ollama.New(rt, hc)
	olHandler.SetTimeout(time.Duration(cfg.Retry.UpstreamTimeout) * time.Second)
	pxHandler := proxy.New(cfg, rt, hc)
	gmHandler := gemini.New(rt, hc)
	gmHandler.SetTimeout(time.Duration(cfg.Retry.UpstreamTimeout) * time.Second)
	ff := proxy.NewFastFailCache(time.Duration(cfg.Retry.FastFailMinutes) * time.Minute)
	pxHandler.SetFastFail(ff)
	admHandler.SetFastFail(ff)
	// 对话调试（/admin/chat）注入完整转发链路与路由探测器：走完整网关路由链路
	admHandler.SetProxy(pxHandler, rt)
	stopProbe := pxHandler.StartFastFailProbe(time.Duration(cfg.Retry.FastFailProbeMinutes) * time.Minute)
	_ = stopProbe
	tz := proxy.NewTokenizer()
	pxHandler.SetTokenizer(tz)
	if dl, derr := storeInst.ListDiscounts(); derr == nil {
		pxHandler.SetDiscounts(dl)
	}
	// 下游 API Key 展示名解析器：从请求头取 Bearer token → auth.Name（按 Key 统计用）
	keyNameFn := func(r *http.Request) string {
		if apiKeys == nil {
			return ""
		}
		token := ""
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			token = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		} else if xk := r.Header.Get("x-api-key"); xk != "" {
			token = strings.TrimSpace(xk)
		}
		return apiKeys.Name(token)
	}
	if rec != nil {
		olHandler.SetRecorder(rec)
		pxHandler.SetRecorder(rec)
		gmHandler.SetRecorder(rec)
		olHandler.SetKeyName(keyNameFn)
		pxHandler.SetKeyName(keyNameFn)
		gmHandler.SetKeyName(keyNameFn)
		// 配额实时用量：转发放行并记录后，同步累加 (api_key, model) 内存窗口计数
		rec.SetUsageHook(func(r store.Record) {
			if qm != nil && r.Tokens > 0 {
				qm.AddUsage(r.APIKey, r.Model, r.Tokens)
			}
		})
	}
	// 模型单价查询：按上游真实模型名定价（默认价兜底在 store.PriceFor 内部）
	pxHandler.SetPriceFor(storeInst.PriceFor)
	// 计费初始化：所有模型默认按 deepseek-v4-flash 定价
	// （输入缓存命中 0.02 元/M，输入缓存未命中 1 元/M，输出 2 元/M）
	storeInst.EnsureDefaultPrice()
	// ⚠ 2026-08-02 用户拍板：对外只暴露 OpenAI 协议 + Anthropic 协议，
	// 不提供 Ollama 原生入口（/api/chat /api/generate /api/embed）。
	// ollama 类型上游仍支持——通过 /v1/* OpenAI 入口自动转换（dispatchOpenAI/dispatchEmbeddings）。

	// OpenAI 兼容入口：按路由结果分发（ollama 上游走转换，其余走 proxy）
	mux.HandleFunc("GET /v1/models", apiKeys.Middleware(admHandler.Models))
	mux.HandleFunc("POST /v1/chat/completions", apiKeys.Middleware(qm.Middleware(dispatchOpenAI(rt, olHandler, pxHandler, gmHandler))))
	// 老版 OpenAI 兼容接口（text 补全）：直接走 proxy 转换转发，ollama 上游兼容暂不考虑
	mux.HandleFunc("POST /v1/completions", apiKeys.Middleware(qm.Middleware(pxHandler.Completions)))
	mux.HandleFunc("POST /v1/embeddings", apiKeys.Middleware(qm.Middleware(dispatchEmbeddings(rt, olHandler, pxHandler))))
	mux.HandleFunc("POST /v1/rerank", apiKeys.Middleware(qm.Middleware(pxHandler.Rerank)))
	antHandler := anthropic.New(rt, hc)
	antHandler.SetTimeout(time.Duration(cfg.Retry.UpstreamTimeout) * time.Second)
	if rec != nil {
		antHandler.SetRecorder(rec)
	}
	mux.HandleFunc("POST /v1/messages", apiKeys.Middleware(qm.Middleware(antHandler.Messages)))

	// Gemini 上游：由 dispatchOpenAI 在路由命中 gemini 类型上游时调用转换（见上方）。
	mux.HandleFunc("POST /v1/images/generations", apiKeys.Middleware(qm.Middleware(pxHandler.ImageGenerations)))
	mux.HandleFunc("POST /v1/audio/speech", apiKeys.Middleware(qm.Middleware(pxHandler.AudioSpeech)))
	mux.HandleFunc("POST /v1/audio/transcriptions", apiKeys.Middleware(qm.Middleware(pxHandler.AudioTranscriptions)))
	mux.HandleFunc("POST /v1/videos", apiKeys.Middleware(qm.Middleware(pxHandler.VideoCreate)))
	mux.HandleFunc("GET /v1/videos", apiKeys.Middleware(qm.Middleware(pxHandler.VideoQuery)))

	return mux
}

// reloadConfig 从 DB 重新加载配置并重建所有组件。
func reloadConfig(storeInst *store.Store, rec *store.Recorder) error {
	state.mu.RLock()
	oldHC := state.hc
	state.mu.RUnlock()

	newCfg, err := config.LoadFromDB(storeInst)
	if err != nil {
		return fmt.Errorf("load from db: %w", err)
	}
	// CLI 端口覆盖数据库配置
	newCfg.Server.Port = *flagPort

	// 关闭旧的 health checker
	if oldHC != nil {
		oldHC.Close()
	}

	// 创建新组件
	rt := router.New(newCfg)
	hc := health.New(newCfg)
	hc.SetProbeRecorder(probeRecorder(rec.Store()))
	hc.Start()

	state.mu.Lock()
	state.hc = hc
	state.mu.Unlock()

	// 创建新 handler
	apiKeys := auth.New(storeInst, admJWTSecret(storeInst))
	if state.qm != nil {
		state.qm.Refresh()
	}
	newHandler := buildServeMux(newCfg, rt, hc, rec, storeInst, apiKeys, state.qm)

	// 替换 srv.Handler
	state.mu.Lock()
	if state.srv != nil {
		state.srv.Handler = newHandler
	}
	state.mu.Unlock()

	// 更新全局 cfg 引用
	cfg = newCfg

	slog.Info("config reloaded", "upstreams", len(newCfg.Upstreams), "rules", len(newCfg.Routing.Rules))
	return nil
}

// dispatchOpenAI 按请求 model 的路由结果分发 /v1/chat/completions：
// 命中 ollama 上游走 ollama 协议转换，命中 gemini 上游走 Gemini 转换，
// 其余走 OpenAI proxy 转发。
func dispatchOpenAI(rt *router.Router, ol *ollama.Handler, px *proxy.Handler, gm *gemini.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, `{"error":{"message":"read body failed","type":"invalid_request_error"}}`, http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Model == "" {
			px.ChatCompletions(w, r)
			return
		}
		ups, _, err := rt.Route(req.Model)
		if err != nil {
			px.ChatCompletions(w, r) // 让 proxy 返回标准 404
			return
		}
		// 第一个候选（按 tier+priority 排序）是 ollama 则走转换
		if len(ups) > 0 && ups[0].IsOllama() {
			ol.OpenAICompletions(w, r)
			return
		}
		if len(ups) > 0 && ups[0].IsGemini() {
			gm.OpenAICompletions(w, r)
			return
		}
		px.ChatCompletions(w, r)
	}
}

// dispatchEmbeddings 处理 /v1/embeddings 分发：路由结果第一个候选是 ollama 走转换，
// 否则走 OpenAI proxy 转发（修复：此前非 ollama 上游嵌入请求被 ollama handler 502）。
func dispatchEmbeddings(rt *router.Router, ol *ollama.Handler, px *proxy.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, `{"error":{"message":"read body failed","type":"invalid_request_error"}}`, http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Model == "" {
			px.Embeddings(w, r)
			return
		}
		ups, _, err := rt.Route(req.Model)
		if err != nil {
			px.Embeddings(w, r) // 让 proxy 返回标准 404
			return
		}
		// 第一个候选（按 tier+priority 排序）是 ollama 则走转换
		if len(ups) > 0 && ups[0].IsOllama() {
			ol.OpenAIEmbeddings(w, r)
			return
		}
		px.Embeddings(w, r)
	}
}

// buildDate 构建日期，发布时通过 -ldflags "-X main.buildDate=2026-08-03" 注入；
// 开发/本地运行时保持 "dev"。
var buildDate = "dev"

// healthzHandler 返回服务健康状态。
func healthzHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "build_date": buildDate})
}

// ensureAdminAPIKey 确保管理 API key（config 表 admin.api_key）存在。
// 首次启动生成随机 key（xjk- 前缀 + 32 hex），存 DB；用户可在系统设置页查看/修改。
func ensureAdminAPIKey(s *store.Store) error {
	all, err := s.GetAllConfig()
	if err != nil {
		return err
	}
	if v, ok := all["admin.api_key"]; ok && strings.TrimSpace(v) != "" {
		return nil
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	key := "xjk-" + hex.EncodeToString(b)
	if err := s.SetConfig("admin.api_key", key); err != nil {
		return err
	}
	slog.Info("generated admin api key", "key", key)
	return nil
}
