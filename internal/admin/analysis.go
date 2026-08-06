// 客户端程序分析：识别 request_log 中每个 client_addr（IP:port）对应的调用程序。
//
// 流程：定时器/手动触发 → 聚合最近 N 分钟去重 client_addr → 只排除无意义地址
// （本机地址优先分析，因为只有本机端口能查到进程）→ 逐个分析：优先按 User-Agent
// 判定（客户端自报身份，最强信号，零成本），UA 未命中再按端口查本机进程
// （ss -tnp / lsof -i :PORT + /proc/<pid>/cmdline）做确定性识别，两者都失败则标记
// 未知 → 结果经应用层 API upsert 到 client_profiles 表 → 前端"客户端分析"页展示。
// 注意：不再调用 pi CLI 推断（用户决策 2026-08-07，UA 识别零成本且更快）。
package admin

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"log/slog"

	"github.com/icefairy/xuanji/internal/config"
	"github.com/icefairy/xuanji/internal/store"
)

// ClientAnalyzer 是客户端程序分析器：按配置间隔定时（或手动）聚合 request_log
// 中的 client_addr，按 User-Agent → 端口查进程的顺序识别调用程序并落库 client_profiles。
type ClientAnalyzer struct {
	cfg   *config.Config
	store *store.Store
	// piRunner 可替换的识别函数（测试注入用，替换整个 UA/端口查进程流程以保证测试确定性）；
	// nil 时走真实识别流程。名字保留 piRunner 仅为兼容既有测试注入接口，与 pi CLI 无关。
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

// RunOnce 执行一次完整分析：聚合最近 N 分钟去重 client_addr → 只排除无意义地址
// （本机地址优先分析，因为只有本机端口能查到进程）→ 逐个分析（UA 判定优先，未命中
// 再按端口查进程，都失败标记未知，不调 pi）→ upsert 到 client_profiles。manual 表示是否手动触发。
func (a *ClientAnalyzer) RunOnce(manual bool) (analysisRunResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.Enabled() {
		return analysisRunResponse{Status: "error", Message: "客户端分析未开启，请在系统设置中打开"}, fmt.Errorf("client analysis disabled")
	}

	// 窗口：config 的 client_analysis_interval 单位是秒（用户设 1 分钟 → 60），
	// 换算成分钟（最小 1，最大 60）。
	minutes := a.cfg.Proxy.ClientAnalysisInterval / 60
	if minutes < 1 {
		minutes = 1
	}
	if minutes > 60 {
		minutes = 60
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
	// 2. 只排除无意义地址（空串/0.0.0.0/::）；本机地址优先分析（端口能查进程），远程随后
	var locals, remotes []string
	for _, addr := range addrs {
		if isMeaninglessAddr(addr) {
			continue
		}
		if isLocalIP(addrHost(addr)) {
			locals = append(locals, addr)
		} else {
			remotes = append(remotes, addr)
		}
	}
	targets := append(locals, remotes...)
	slog.Info("client analysis candidates", "total", len(addrs), "after_filter", len(targets), "local", len(locals), "remote", len(remotes))

	// 3. 拉取请求特征作为推断线索（模型/端点/请求数）
	feats, err := a.store.GetClientAddrFeatures(minutes)
	if err != nil {
		return analysisRunResponse{Status: "error", Message: err.Error()}, err
	}
	featMap := make(map[string]store.ClientAddrFeature, len(feats))
	for _, f := range feats {
		featMap[f.ClientAddr] = f
	}

// 4. 并发分析（最多 maxAnalyzePerRun 个）：UA 判定与端口查进程都是快速本地操作
// （ss 全量输出在连接多时可能到秒级），4 路并发 + 总超时 240s 兜底，保证手动触发
// 在 HTTP 超时前返回；结果经 channel 汇总后由本 goroutine 顺序 Upsert
// （SQLite 写并发会锁，避免 write lock 冲突）。
newProfiles := 0
analyzeCtx, cancel := context.WithTimeout(context.Background(), runTotalTimeout)
defer cancel()

// analyzeResult 单地址分析结果（channel 汇总用）。
type analyzeResult struct {
		addr       string
		program    string
		confidence float64
		evidence   string
		err        error
}
results := make(chan analyzeResult, maxAnalyzePerRun)
sem := make(chan struct{}, analyzeConcurrency) // 信号量：限制并发数

var wg sync.WaitGroup
count := 0
for _, addr := range targets {
		if count >= maxAnalyzePerRun {
			slog.Warn("client analysis reached per-run cap", "cap", maxAnalyzePerRun)
			break
		}
		count++
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			// 总超时后不再启动新分析（信号量获取也受 ctx 控制）
			select {
			case sem <- struct{}{}:
			case <-analyzeCtx.Done():
						return
			}
			defer func() { <-sem }()
			program, confidence, evidence, err := a.analyzeAddr(analyzeCtx, addr, featMap[addr])
			results <- analyzeResult{addr: addr, program: program, confidence: confidence, evidence: evidence, err: err}
		}(addr)
}
go func() {
		wg.Wait()
		close(results)
}()

// 顺序落库（仅本 goroutine 写 SQLite）
for res := range results {
		if res.err != nil {
			slog.Warn("client analysis addr failed", "client_addr", res.addr, "error", res.err)
			continue
		}
		if err := a.store.UpsertClientProfile(store.ClientProfile{
			ClientAddr: res.addr,
			Program:    res.program,
			Confidence: res.confidence,
			Evidence:   res.evidence,
		}); err != nil {
			slog.Warn("client analysis upsert failed", "client_addr", res.addr, "error", err)
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

// analyzeConcurrency 并发分析 goroutine 数：端口查进程（ss 全量输出）可能到秒级，
// 4 路并发避免 30 个地址串行拖慢手动触发。
const analyzeConcurrency = 4

// runTotalTimeout 单次 RunOnce 总超时：240s 上限保证手动触发能在 HTTP 超时
// （前端 fetch 默认）前返回；到点后未启动的地址本轮跳过。
const runTotalTimeout = 240 * time.Second

// analyzeAddr 分析单个 client_addr，按信号强度依次判定（不再调 pi CLI）：
//  1. User-Agent（最强信号，客户端自报身份：Hermes/x.x.x、claude-cli/x.x.x、pi/x.x.x）
//  2. 端口查本机进程 + cmdline（确定性识别，ss -tnp / lsof + /proc/<pid>/cmdline）
//  3. 都失败 → 标记未知（confidence 0.1）
func (a *ClientAnalyzer) analyzeAddr(ctx context.Context, addr string, feat store.ClientAddrFeature) (program string, confidence float64, evidence string, err error) {
	// 测试注入：替换整个推断流程（含 UA/端口查进程），保证测试确定性
	if a.piRunner != nil {
		return a.piRunner(addr, feat)
	}
	// 第一步：User-Agent 判定（最强识别信号，零成本，无需查进程）
	if feat.UserAgents != "" {
		if program, confidence, evidence, ok := classifyFromUA(feat.UserAgents); ok {
			return program, confidence, evidence, nil
		}
	}
	// 第二步：从 addr 提取端口，查本机端口对应进程
	port := portFromAddr(addr)
	if port != "" {
		if proc := lookupProcessByPort(port); proc.PID != "" {
			// cmdline 有明确程序标识 → 直接判定
			if program, confidence, evidence, ok := classifyFromCmdline(port, proc); ok {
				return program, confidence, evidence, nil
			}
			// cmdline 模糊（如裸 python3/java 无脚本名）→ 无法确定程序
			return "未知", 0.1, fmt.Sprintf("端口 %s → %s(pid=%s) 但 cmdline 模糊，无法确定程序", port, proc.Name, proc.PID), nil
		}
	}
	// 第三步：UA 未识别且查不到进程 → 未知（不再调 pi）
	return "未知", 0.1, "UA 未识别且端口无进程", nil
}

// ===== UA 判定（最强识别信号） =====

// uaRules 已知 User-Agent 规则 → 程序名/置信度。
// UA 是客户端自报身份的强信号（Hermes 请求头是 "Hermes/x.x.x"，claude-cli 是
// "claude-cli/x.x.x"，pi 是 "pi/x.x.x"，curl 是 "curl/x.x.x"），命中即确定性识别，
// 无需查进程、无需调 pi。规则与任务文档表格一一对应：带斜杠的条目前缀匹配，
// 不带斜杠的条目（opencode/codex/cursor/cline/cherry 等）子串匹配，兼容大小写变体。
// 前缀/子串都按小写比较；"pi" 必须前缀 "pi/" 或子串 "pi-agent"，避免误伤
// python/pip/api 等含 "pi" 子串的 UA（如 apifox）。
var uaRules = []struct {
	key     string // 匹配关键字（小写）
	contain bool   // true=子串包含匹配；false=前缀匹配
	program string
	conf    float64
}{
	// 带斜杠：前缀匹配（任务文档表格）
	{"hermes/", false, "Hermes", 0.95},
	{"claude-cli/", false, "Claude Code", 0.95},
	{"claude-code/", false, "Claude Code", 0.95},
	{"pi/", false, "pi agent", 0.95},
	{"curl/", false, "curl", 0.95},
	{"wget/", false, "wget", 0.95},
	{"openai/", false, "OpenAI SDK", 0.9},
	// 不带斜杠：子串包含匹配（任务文档表格）
	{"pi-agent", true, "pi agent", 0.95},
	{"opencode", true, "OpenCode", 0.95},
	{"codex", true, "Codex", 0.9},
	{"cursor", true, "Cursor", 0.9},
	{"cline", true, "Cline", 0.9},
	{"cherry", true, "Cherry Studio", 0.85},
	{"python-requests", true, "python (requests)", 0.9},
	{"node-fetch", true, "node (fetch)", 0.9},
	{"go-http-client", true, "Go HTTP", 0.85},
	// 表格外补充规则（增强识别，与表格不冲突）
	{"chatgpt/", false, "ChatGPT", 0.9},
	{"okhttp/", false, "okhttp", 0.8},
	{"python-urllib/", false, "python", 0.9},
	{"openai-python/", false, "OpenAI SDK (python)", 0.8},
	{"openai-node/", false, "OpenAI SDK (node)", 0.8},
}

// classifyFromUA 从 User-Agent 直接判定程序名。ua 可以是单个 UA 或逗号分隔的
// UA 集合（GetClientAddrFeatures 的 GROUP_CONCAT(DISTINCT user_agent) 结果），
// 逐个尝试已知规则（前缀或子串匹配），命中即返回 (program, confidence, evidence, true)。
// 全部未知（含空 UA）时返回 ok=false，由调用方继续走端口查进程。
func classifyFromUA(ua string) (program string, confidence float64, evidence string, ok bool) {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return "", 0, "", false
	}
	for _, part := range strings.Split(ua, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lower := strings.ToLower(part)
		for _, rule := range uaRules {
			matched := false
			if rule.contain {
				matched = strings.Contains(lower, rule.key)
			} else {
				matched = strings.HasPrefix(lower, rule.key)
			}
			if matched {
				return rule.program, rule.conf, "User-Agent: " + part, true
			}
		}
	}
	return "", 0, "", false
}

// ===== 确定性识别：端口 → 进程 → cmdline =====

// procInfo 是本机端口对应的进程信息（确定性识别依据）。
type procInfo struct {
	Name    string // 进程名（ss/lsof 的 COMMAND 列）
	PID     string // 进程号
	Cmdline string // 启动命令（/proc/<pid>/cmdline，NUL 分隔转空格；可能为空）
}

// ssProcRe 匹配 ss -tnp 输出的进程信息列 users:(("进程名",pid=NNNN,fd=FF))。
// 注意：ss 实际输出是双左括号，正则必须写成 \(\(（单反斜杠）；写成 \\( 会把 \\
// 解析为"匹配反斜杠字符"，导致编译错误或永远不匹配。
var ssProcRe = regexp.MustCompile(`users:\(\("([^"]+)",pid=(\d+),`)

// piWordRe 词边界匹配 "pi"（pi agent 的启动命令），避免误伤 python/pip/api 等。
var piWordRe = regexp.MustCompile(`\bpi\b`)

// portLookupTimeout 单次端口→进程查询超时（ss 全量输出在连接多时可能较慢）。
const portLookupTimeout = 5 * time.Second

// lookupProcessByPort 查本机端口对应的进程（确定性识别核心）。
// 优先 ss -tnp（输出含 Local/Peer 与进程信息），失败或查不到时回退 lsof -i :PORT；
// 查到进程后读 /proc/<pid>/cmdline 拿启动命令。单次查询 5 秒超时；
// 查不到（端口空闲/无本机 socket/工具不可用/权限不足）返回空 procInfo，不报错。
func lookupProcessByPort(port string) procInfo {
	if port == "" {
		return procInfo{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), portLookupTimeout)
	defer cancel()

	if out, err := exec.CommandContext(ctx, "ss", "-tnp").Output(); err == nil {
		if p := parseSSOutput(string(out), port); p.PID != "" {
			p.Cmdline = readCmdline(p.PID)
			return p
		}
	}
	if out, err := exec.CommandContext(ctx, "lsof", "-i", ":"+port, "-P", "-n").Output(); err == nil {
		if p := parseLsofOutput(string(out), port); p.PID != "" {
			p.Cmdline = readCmdline(p.PID)
			return p
		}
	}
	return procInfo{}
}

// parseSSOutput 解析 ss -tnp 输出，找到端口 port 对应的进程。
// ss 数据行格式（空白分隔，6 列，Local=fields[3]、Peer=fields[4]）：
//
//	ESTAB  0  0  192.168.1.10:11314  192.168.1.10:3001  users:(("pi",pid=585844,fd=31))
//
// client_addr 的本地端口可能出现在 Local 或 Peer 任意一侧，两边都匹配。
func parseSSOutput(out, port string) procInfo {
	for _, line := range strings.Split(out, "\n") {
		// 跳过表头与无进程信息行（TIME_WAIT 等无主 socket 没有 users: 列）
		if !strings.Contains(line, "users:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		if portOfEndpoint(fields[3]) != port && portOfEndpoint(fields[4]) != port {
			continue
		}
		if m := ssProcRe.FindStringSubmatch(line); m != nil {
			// pid=0 是无主 socket 的内核占位，不算真实进程
			if m[2] == "0" {
				continue
			}
			return procInfo{Name: m[1], PID: m[2]}
		}
	}
	return procInfo{}
}

// parseLsofOutput 解析 `lsof -i :PORT -P -n` 输出，取第一行数据行。
// 数据行格式：COMMAND PID USER FD TYPE DEVICE SIZE/OFF NODE NAME
//
//	docker-prox 1208261 root 26u IPv4 385095 0t0 TCP 192.168.1.10:6379 (LISTEN)
//
// NAME 列含空格（如 "TCP 127.0.0.1:3981 (LISTEN)"）会被拆成多列，
// 因此按整行匹配目标端口，而不是只查最后一列。
func parseLsofOutput(out, port string) procInfo {
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, ":"+port) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 9 || fields[0] == "COMMAND" {
			continue
		}
		pid := fields[1]
		if pid == "0" {
			continue
		}
		if _, err := strconv.Atoi(pid); err != nil {
			continue
		}
		return procInfo{Name: fields[0], PID: pid}
	}
	return procInfo{}
}

// readCmdline 读 /proc/<pid>/cmdline 启动命令：NUL 分隔的参数转空格。
// 权限不足或进程已退出返回 ""。
func readCmdline(pid string) string {
	if pid == "" || pid == "0" {
		return ""
	}
	data, err := os.ReadFile("/proc/" + pid + "/cmdline")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.ReplaceAll(string(data), "\x00", " "))
}

// classifyFromCmdline 从进程启动命令中确定性识别程序名（不再调 pi）。
// cmdline 含明确程序标识（pi/node/hermes/claude/curl/python 脚本名等）时返回
// (program, confidence, evidence, true)；裸解释器（python3/java 等无脚本名、
// 无标识）返回 ok=false，由调用方标记未知。
func classifyFromCmdline(port string, proc procInfo) (program string, confidence float64, evidence string, ok bool) {
	lowerName := strings.ToLower(proc.Name)
	lowerCmd := strings.ToLower(proc.Cmdline)

	// 已知程序标识：进程名或 cmdline 出现即认定
	known := []struct {
		keys    []string
		program string
		conf    float64
	}{
		{[]string{"hermes"}, "Hermes", 0.9},
		{[]string{"claude"}, "Claude Code", 0.9},
		{[]string{"codex"}, "Codex", 0.9},
		{[]string{"cursor"}, "Cursor", 0.9},
		{[]string{"opencode"}, "OpenCode", 0.9},
		{[]string{"cline"}, "Cline", 0.9},
		{[]string{"cherry studio", "cherrystudio"}, "Cherry Studio", 0.85},
		{[]string{"curl"}, "curl", 0.9},
		{[]string{"wget"}, "wget", 0.9},
	}
	for _, k := range known {
		for _, key := range k.keys {
			if strings.Contains(lowerName, key) || strings.Contains(lowerCmd, key) {
				return k.program, k.conf, portProcEvidence(port, proc), true
			}
		}
	}
	// pi agent：词边界匹配（进程名恰为 pi，或 cmdline 中出现独立单词 pi）
	if lowerName == "pi" || piWordRe.MatchString(lowerCmd) {
		return "pi agent", 0.9, portProcEvidence(port, proc), true
	}
	// 脚本文件参数：python3 /opt/agent.py --server → agent（可信度略低）
	if script := scriptArg(lowerCmd); script != "" {
		return script, 0.85, portProcEvidence(port, proc), true
	}
	return "", 0, "", false
}

// scriptExts 视为脚本文件的扩展名列表（cmdline 中出现即用脚本名当程序名）。
var scriptExts = []string{".py", ".js", ".mjs", ".ts", ".sh", ".rb", ".pl"}

// scriptArg 从 cmdline 中找脚本文件参数（.py/.js/.ts/.sh 等），
// 返回脚本名（不含扩展名）作为可读程序名；找不到返回 ""。
func scriptArg(lowerCmd string) string {
	for _, tok := range strings.Fields(lowerCmd) {
		base := path.Base(tok)
		for _, ext := range scriptExts {
			if strings.HasSuffix(base, ext) {
				name := strings.TrimSuffix(base, ext)
				if name != "" {
					return name
				}
			}
		}
	}
	return ""
}

// portProcEvidence 构造确定性识别的证据："端口 xxx → 进程名(pid) → cmdline: ..."。
func portProcEvidence(port string, proc procInfo) string {
	ev := fmt.Sprintf("端口 %s → %s(pid=%s)", port, proc.Name, proc.PID)
	if proc.Cmdline != "" {
		ev += " → cmdline: " + proc.Cmdline
	}
	return ev
}

// portOfEndpoint 从 ss 输出的端点串（"IP:端口"）提取端口。
// 兼容 IPv4 "1.2.3.4:80"、通配 "*:80"、IPv6 "[::1]:80"；提取失败返回 ""。
func portOfEndpoint(ep string) string {
	ep = strings.TrimSpace(ep)
	if ep == "" {
		return ""
	}
	// IPv6 带方括号：[::1]:80
	if i := strings.LastIndex(ep, "]:"); i >= 0 {
		return ep[i+2:]
	}
	// 通配监听：*:80
	if strings.HasPrefix(ep, "*:") {
		return ep[2:]
	}
	// IPv4 / 主机名：1.2.3.4:80 / localhost:80
	if i := strings.LastIndex(ep, ":"); i >= 0 {
		p := ep[i+1:]
		if p != "" && isAllDigits(p) {
			return p
		}
	}
	return ""
}

// isAllDigits 判断字符串是否全为数字。
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// ===== 地址判断 =====

// isMeaninglessAddr 判断 client_addr 是否为无意义地址（应排除，不分析）。
// 只排除空串、0.0.0.0、:: 等明显无意义项；本机地址（127.0.0.1/::1/本机网卡 IP）
// 保留——本机端口才能查到进程，是确定性识别的核心输入。
func isMeaninglessAddr(addr string) bool {
	addr = strings.TrimSpace(addr)
	if addr == "" || addr == "::" {
		return true
	}
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	} else if i := strings.LastIndex(addr, ":"); i > 0 {
		host = addr[:i]
	}
	switch strings.Trim(host, "[]") {
	case "0.0.0.0", "::":
		return true
	}
	return false
}

// addrHost 提取 client_addr 的主机部分（兼容无方括号裸 IPv6 "::1:53211"）。
func addrHost(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return addr[:i]
	}
	return addr
}

// isLocalIP 判断 IP 是否为本机：回环（127.0.0.1/::1）或本机网卡 IP。
func isLocalIP(ipStr string) bool {
	ip := net.ParseIP(strings.TrimSpace(ipStr))
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	if ifaces, err := net.InterfaceAddrs(); err == nil {
		for _, a := range ifaces {
			if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.Equal(ip) {
				return true
			}
		}
	}
	return false
}

// portFromAddr 从 client_addr（IP:port）提取端口；SplitHostPort 失败
// （如无方括号裸 IPv6 "::1:53211"）按最后一个冒号兜底切端口。
func portFromAddr(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil {
		return port
	}
	if i := strings.LastIndex(addr, ":"); i > 0 && i < len(addr)-1 {
		p := addr[i+1:]
		if n, err := strconv.Atoi(p); err == nil && n > 0 && n <= 65535 {
			return p
		}
	}
	return ""
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

// profileWithCacheStats 是客户端分析档案 + 缓存命中率统计的响应体（GET /admin/analysis/profiles）。
// 嵌入 store.ClientProfile 展开原有字段；cache_max/min/avg 为百分比（0-100），
// 无有效统计时为 null（前端显示 '-'）。
type profileWithCacheStats struct {
	store.ClientProfile
	CacheMax *float64 `json:"cache_max"` // 最大缓存命中率（百分比），无数据显示 null
	CacheMin *float64 `json:"cache_min"` // 最小缓存命中率（百分比），无数据显示 null
	CacheAvg *float64 `json:"cache_avg"` // 平均缓存命中率（百分比），无数据显示 null
}

// AnalysisProfiles 返回全部客户端程序分析档案（GET /admin/analysis/profiles）。
// 每条档案附带该 client_addr 的缓存命中率统计（cache_max/cache_min/cache_avg，
// 百分比 0-100）；无统计数据的地址对应字段为 null。
func (h *Handler) AnalysisProfiles(w http.ResponseWriter, _ *http.Request) {
	if h.store == nil {
		writeJSON(w, []profileWithCacheStats{})
		return
	}
	profiles, err := h.store.ListClientProfiles()
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	// 缓存命中率聚合（按 client_addr 匹配，一次查询全量）
	cacheStats, err := h.store.ClientProfileCacheStats()
	if err != nil {
		slog.Warn("client profile cache stats failed", "error", err)
		cacheStats = nil
	}
	out := make([]profileWithCacheStats, 0, len(profiles))
	for _, p := range profiles {
		item := profileWithCacheStats{ClientProfile: p}
		if st, ok := cacheStats[p.ClientAddr]; ok {
			maxR, minR, avgR := st.Max, st.Min, st.Avg
			item.CacheMax = &maxR
			item.CacheMin = &minR
			item.CacheAvg = &avgR
		}
		out = append(out, item)
	}
	writeJSON(w, out)
}
