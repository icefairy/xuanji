// 客户端程序分析：识别 request_log 中每个 client_addr（IP:port）对应的调用程序。
//
// 流程：定时器/手动触发 → 聚合最近 N 分钟去重 client_addr → 过滤本机 IP
// → 逐个调 pi CLI（pi -p --session-id ... @任务文件）推断程序名
// → 结果经应用层 API upsert 到 client_profiles 表 → 前端"客户端分析"页展示。
package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"log/slog"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/store"
)

// ClientAnalyzer 是客户端程序分析器：按配置间隔定时（或手动）聚合 request_log
// 中的 client_addr，调 pi CLI 推断对应程序并落库 client_profiles。
type ClientAnalyzer struct {
	cfg   *config.Config
	store *store.Store
	// piRunner 可替换的 pi 调用实现（测试注入用）；nil 时走真实 pi CLI。
	piRunner func(addr string, feat store.ClientAddrFeature) (string, float64, string, error)

	mu      sync.Mutex // 防止定时触发与手动触发并发执行分析
	quit    chan struct{}
	wg      sync.WaitGroup
	stopped bool
}

// NewClientAnalyzer 创建分析器。cfg 为 nil 或 store 为 nil 时不执行分析。
func NewClientAnalyzer(cfg *config.Config, st *store.Store) *ClientAnalyzer {
	return &ClientAnalyzer{cfg: cfg, store: st, quit: make(chan struct{})}
}

// Enabled 返回客户端分析开关是否开启（未开启时手动触发拒绝执行）。
func (a *ClientAnalyzer) Enabled() bool {
	return a.cfg != nil && a.store != nil && a.cfg.Proxy.ClientAnalysis
}

// Start 按配置启动定时分析（goroutine + ticker）。开关关闭或 store 为 nil 时不启动。
func (a *ClientAnalyzer) Start() {
	if !a.Enabled() {
		return
	}
	interval := a.cfg.Proxy.ClientAnalysisInterval
	if interval < 60 {
		interval = 600 // 最小 1 分钟，默认 10 分钟
	}
	a.wg.Add(1)
	go a.loop(interval)
}

// Stop 停止定时分析并等待后台协程退出；可安全重复调用。
func (a *ClientAnalyzer) Stop() {
	a.mu.Lock()
	if !a.stopped {
		a.stopped = true
		close(a.quit)
	}
	a.mu.Unlock()
	a.wg.Wait()
}

// loop 是定时分析主循环：按 interval 秒触发一次 RunOnce。
func (a *ClientAnalyzer) loop(interval int) {
	defer a.wg.Done()
	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-a.quit:
			return
		case <-ticker.C:
			if _, err := a.RunOnce(false); err != nil {
				slog.Warn("client analysis failed", "error", err)
			}
		}
	}
}

// analysisRunResponse 是分析执行结果的响应体。
type analysisRunResponse struct {
	Status      string `json:"status"`       // ok / error
	Analyzed    int    `json:"analyzed"`     // 尝试分析的 client_addr 数
	NewProfiles int    `json:"new_profiles"` // 成功得到分析结果并落库的条数
	Message     string `json:"message,omitempty"`
}

// RunOnce 执行一次完整分析：聚合最近 N 分钟去重 client_addr → 过滤本机 IP
// → 逐个调 pi CLI 推断 → upsert 到 client_profiles。manual 表示是否手动触发。
func (a *ClientAnalyzer) RunOnce(manual bool) (analysisRunResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.Enabled() {
		return analysisRunResponse{Status: "error", Message: "客户端分析未开启，请在系统设置中打开"}, fmt.Errorf("client analysis disabled")
	}

	minutes := a.cfg.Proxy.ClientAnalysisInterval
	if minutes < 1 {
		minutes = 10
	}
	if manual {
		slog.Info("client analysis start", "window_minutes", minutes)
	} else {
		slog.Debug("client analysis start", "window_minutes", minutes)
	}

	// 1. 聚合最近 N 分钟去重 client_addr
	addrs, err := a.store.GetDistinctClientAddrs(minutes)
	if err != nil {
		return analysisRunResponse{Status: "error", Message: err.Error()}, err
	}
	// 2. 过滤本机 IP
	var targets []string
	locals := localIPs()
	for _, addr := range addrs {
		if isLocalAddr(addr, locals) {
			continue
		}
		targets = append(targets, addr)
	}
	slog.Info("client analysis candidates", "total", len(addrs), "after_filter", len(targets))

	// 3. 拉取请求特征作为推断线索（模型/端点/请求数）
	feats, err := a.store.GetClientAddrFeatures(minutes)
	if err != nil {
		return analysisRunResponse{Status: "error", Message: err.Error()}, err
	}
	featMap := make(map[string]store.ClientAddrFeature, len(feats))
	for _, f := range feats {
		featMap[f.ClientAddr] = f
	}

	// 4. 逐个调 pi CLI 分析并落库（最多分析 maxAnalyzePerRun 个，避免一次拖太久）
	newProfiles := 0
	ctx := context.Background()
	for i, addr := range targets {
		if i >= maxAnalyzePerRun {
			slog.Warn("client analysis reached per-run cap", "cap", maxAnalyzePerRun)
			break
		}
		program, confidence, evidence, err := a.analyzeAddr(ctx, addr, featMap[addr])
		if err != nil {
			slog.Warn("client analysis addr failed", "client_addr", addr, "error", err)
			continue
		}
		if err := a.store.UpsertClientProfile(store.ClientProfile{
			ClientAddr: addr,
			Program:    program,
			Confidence: confidence,
			Evidence:   evidence,
		}); err != nil {
			slog.Warn("client analysis upsert failed", "client_addr", addr, "error", err)
			continue
		}
		newProfiles++
	}

	resp := analysisRunResponse{
		Status:      "ok",
		Analyzed:    len(targets),
		NewProfiles: newProfiles,
	}
	if manual {
		slog.Info("client analysis done", "analyzed", resp.Analyzed, "new_profiles", resp.NewProfiles)
	} else {
		slog.Debug("client analysis done", "analyzed", resp.Analyzed, "new_profiles", resp.NewProfiles)
	}
	return resp, nil
}

// maxAnalyzePerRun 单次分析最多处理的 client_addr 数（防止大量地址拖垮手动触发）。
const maxAnalyzePerRun = 30

// piSessionID 是调用 pi CLI 分析时固定使用的 session-id（多轮上下文复用）。
const piSessionID = "xuanji-client-analyze"

// piAnalyzeTimeout 单次 pi 调用的超时。
// 120 秒：模型推理本身需 10-30s，且可能与用户自己的 pi 会话排队共享 provider
// 并发槽位，超时太短会在排队时误杀（实测 60s 在并发时不够）。
const piAnalyzeTimeout = 120 * time.Second

// piSystemPrompt 注入 pi 的系统提示：只输出 JSON，不要多余文字。
const piSystemPrompt = "你是一个客户端程序识别器。根据给出的 client_addr 和请求特征，推断调用方是什么 AI 客户端程序（如 Hermes、Claude Code、Cursor、OpenCode、pi agent、Cline、Cherry Studio 等）。只输出一个 JSON 对象，禁止输出任何其他文字、解释或 Markdown 代码块围栏。JSON 格式：{\"program\":\"程序名\",\"confidence\":0.8,\"evidence\":\"推断依据\"}。program 用简短程序名；confidence 是 0-1 的置信度；evidence 用一句话说明依据（端口、UA、调用特征等）。实在无法判断时 program 用\"未知\"，confidence 用 0.1。"

// analyzeAddr 调用 pi CLI 分析单个 client_addr，返回推断的程序名/置信度/依据。
// 按任务文档约定使用 `pi -p --session-id <id> --append-system-prompt "..." @任务文件`
// （任务内容写入临时文件，不通过命令行内联，避免引号转义问题）。
func (a *ClientAnalyzer) analyzeAddr(ctx context.Context, addr string, feat store.ClientAddrFeature) (program string, confidence float64, evidence string, err error) {
	// 测试注入：替换真实 pi CLI 调用
	if a.piRunner != nil {
		return a.piRunner(addr, feat)
	}
	// 生成任务文件
	taskFile, err := os.CreateTemp("", "xuanji-analysis-*.md")
	if err != nil {
		return "", 0, "", fmt.Errorf("create task file: %w", err)
	}
	taskPath := taskFile.Name()
	defer func() {
		_ = taskFile.Close()
		_ = os.Remove(taskPath)
	}()

	var b strings.Builder
	b.WriteString("任务：推断下面这个客户端地址对应的调用程序。\n\n")
	b.WriteString("client_addr: " + addr + "\n")
	if feat.ClientAddr != "" {
		b.WriteString("\n请求特征（网关 request_log 最近窗口聚合）：\n")
		if feat.Models != "" {
			b.WriteString("- 调用模型: " + feat.Models + "\n")
		}
		if feat.Endpoints != "" {
			b.WriteString("- 使用端点: " + feat.Endpoints + "\n")
		}
		b.WriteString(fmt.Sprintf("- 请求次数: %d\n", feat.Requests))
	}
	b.WriteString("- User-Agent: 未知（网关未记录）\n")
	b.WriteString("\n请仅输出 JSON：{\"program\":\"程序名\",\"confidence\":0.8,\"evidence\":\"推断依据\"}\n")
	if _, err := taskFile.WriteString(b.String()); err != nil {
		return "", 0, "", fmt.Errorf("write task file: %w", err)
	}

	ctx2, cancel := context.WithTimeout(ctx, piAnalyzeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx2, "pi",
		"-p",
		"--session-id", piSessionID,
		"--append-system-prompt", piSystemPrompt,
		"@"+taskPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", 0, "", fmt.Errorf("pi 调用失败: %w (输出: %s)", err, truncateStr(string(out), 300))
	}
	program, confidence, evidence, err = parsePiJSON(out)
	if err != nil {
		return "", 0, "", fmt.Errorf("解析 pi 输出失败: %w (输出: %s)", err, truncateStr(string(out), 300))
	}
	return program, confidence, evidence, nil
}

// parsePiJSON 从 pi 输出中宽松提取 JSON 对象并解析（容忍 Markdown 代码块围栏/前后缀文字）。
func parsePiJSON(out []byte) (program string, confidence float64, evidence string, err error) {
	s := strings.TrimSpace(string(out))
	// 去掉 ```json / ``` 代码块围栏（可能成对出现，也可能只有一侧）
	s = strings.ReplaceAll(s, "```", "")
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return "", 0, "", fmt.Errorf("输出中没有 JSON 对象")
	}
	var v struct {
		Program    string  `json:"program"`
		Confidence float64 `json:"confidence"`
		Evidence   string  `json:"evidence"`
	}
	if err := json.Unmarshal([]byte(s[start:end+1]), &v); err != nil {
		return "", 0, "", err
	}
	if v.Confidence < 0 {
		v.Confidence = 0
	}
	if v.Confidence > 1 {
		v.Confidence = 1
	}
	if strings.TrimSpace(v.Program) == "" {
		v.Program = "未知"
	}
	return strings.TrimSpace(v.Program), v.Confidence, strings.TrimSpace(v.Evidence), nil
}

// localIPs 返回网关本机 IP 集合：
// 硬编码的 localhost / 网关自身内网 IP（192.168.1.10 等，见任务文档）+ 本机所有网卡 IP。
func localIPs() map[string]bool {
	set := map[string]bool{
		"127.0.0.1":    true,
		"::1":          true,
		"0.0.0.0":      true,
		"::":           true,
		"localhost":    true,
		"192.168.1.10": true, // 网关自身内网 IP
	}
	if ifaces, err := net.InterfaceAddrs(); err == nil {
		for _, a := range ifaces {
			if ipnet, ok := a.(*net.IPNet); ok {
				set[ipnet.IP.String()] = true
			}
		}
	}
	return set
}

// isLocalAddr 判断 client_addr（IP:port）是否属于网关本机（应过滤，不分析）。
func isLocalAddr(addr string, locals map[string]bool) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	} else if i := strings.LastIndex(addr, ":"); i > 0 {
		// SplitHostPort 失败（如无方括号的裸 IPv6 "::1:53211"），按最后一个冒号切掉端口
		host = addr[:i]
	}
	if locals[host] {
		return true
	}
	// 整体兜底：当纯 IP（无端口）时直接解析匹配
	if ip := net.ParseIP(strings.TrimSpace(addr)); ip != nil {
		return locals[ip.String()]
	}
	return false
}

// ===== Handler 端点 =====

// anaMu 保护 Handler.ana 字段（reload 重建时与请求处理并发安全）。
var anaMu sync.RWMutex

// SetAnalyzer 注入客户端程序分析器（main 启动/reload 时调用）。
func (h *Handler) SetAnalyzer(a *ClientAnalyzer) {
	anaMu.Lock()
	defer anaMu.Unlock()
	h.ana = a
}

// analyzer 返回当前注入的分析器（nil 安全）。
func (h *Handler) analyzer() *ClientAnalyzer {
	anaMu.RLock()
	defer anaMu.RUnlock()
	return h.ana
}

// AnalysisRun 手动触发一次客户端程序分析（POST /admin/analysis/run）。
// 返回 {status, analyzed, new_profiles}。
func (h *Handler) AnalysisRun(w http.ResponseWriter, r *http.Request) {
	a := h.analyzer()
	if a == nil {
		writeJSON(w, analysisRunResponse{Status: "error", Message: "客户端分析器未初始化"})
		return
	}
	if !a.Enabled() {
		writeJSON(w, analysisRunResponse{Status: "error", Message: "客户端分析未开启，请在系统设置中打开"})
		return
	}
	resp, err := a.RunOnce(true)
	if err != nil {
		writeJSON(w, analysisRunResponse{Status: "error", Message: err.Error()})
		return
	}
	writeJSON(w, resp)
}

// AnalysisProfiles 返回全部客户端程序分析档案（GET /admin/analysis/profiles）。
func (h *Handler) AnalysisProfiles(w http.ResponseWriter, _ *http.Request) {
	if h.store == nil {
		writeJSON(w, []store.ClientProfile{})
		return
	}
	profiles, err := h.store.ListClientProfiles()
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, profiles)
}

// 编译期断言：保证文件路径目录可被 go vet 识别（无用导入保护）。
