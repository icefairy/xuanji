# 璇玑 Xuanji —— OpenAI/Claude 协议汇聚·负载·分流网关

> Go + openai-go 库自研轻量网关，供本机各类 AI 工具（Hermes / Claude Code / OpenCode / 自研应用）统一接入。
> 璇玑：北斗第一星，古代天文仪器轴心枢纽——汇聚万向，运转分流。中文名"璇玑"，仓库名 xuanji。
> **仓库**: https://github.com/icefairy/xuanji.git

## 一、项目定位

- **为什么自研**：现有 oneapi/new-api 对 OpenAI 协议兼容不好（流式/工具调用/多模态字段易丢），且太重。本项目轻量、可控、协议严格。
- **用户范围**：自己用，给各种 AI 工具当统一入口。
- **对外形态**：一个本地 HTTP 服务，同时暴露 OpenAI 协议与 Anthropic(Claude) 协议。
- **上游形态**：以 OpenAI 兼容端点为主（vLLM / OpenRouter / 中转站 / OpenCode Go 等）。

## 二、核心需求（已确认）

| # | 需求 | 说明 |
|---|------|------|
| 1 | 汇聚 | 多个 OpenAI 兼容上游，统一入口 |
| 2 | 分流 | 配额余量 / 延迟 / 主备 三种策略都要 |
| 3 | 请求类型 | chat/completions（流式+非流式）、embeddings、images、audio |
| 4 | 协议 | 对外兼容 OpenAI 协议 + Claude 协议（协议转换） |
| 5 | 配置 | 静态 YAML 起步，统计/管理界面下一阶段 |
| 6 | 自研动机 | 练手 + oneapi 兼容性差，掌控协议细节 |

## 三、架构

```
┌──────────┐  OpenAI 协议   ┌──────────────────────┐
│  Hermes   │ ────────────▶ │                      │
│  OpenCode │                │     AI Gateway       │
│ ClaudeCode│  Anthropic 协议│   (Go, :8787)        │
│  其他工具  │ ────────────▶ │                      │
└──────────┘                └──────┬───────────────┘
                                   │ 路由 + 分流 + 熔断 + 协议转换
                   ┌───────────────┼───────────────┐
                   ▼               ▼               ▼
             ┌──────────┐   ┌──────────┐   ┌──────────┐
             │  vLLM    │   │OpenRouter│   │ OpenCode │
             │ qwen3.6  │   │  官方API  │   │ Go 套餐  │
             └──────────┘   └──────────┘   └──────────┘
```

### 请求流转
1. 客户端请求 → 网关入口（OpenAI `/v1/*` 或 Anthropic `/v1/messages`）
2. 网关解析请求体 → 归一化为内部 Request 结构
3. 路由层：按 `model` 字段匹配上游组（静态规则 + 模型映射）
4. 分流层：从上游组内按策略选出一个上游
5. 转发层：openai-go 构造上游请求 → SSE 流式/非流式转发
6. 响应归一化 → 转换回客户端协议（如 Claude 客户端则转 Anthropic 格式）

## 四、技术选型

| 层 | 选型 | 理由 |
|----|------|------|
| 语言 | Go 1.22+ | 并发模型天然适合代理/转发 |
| 模块名 | github.com/icefairy/xuanji | 项目模块名 |
| 上游 SDK | github.com/openai/openai-go | 用户指定，协议严格 |
| 路由 | 标准库 net/http (1.22 增强路由) | 轻量，无框架依赖 |
| SSE | 手写 io.Copy + bufio 流式透传 | 控制力最强 |
| 配置 | YAML (gopkg.in/yaml.v3) | 静态起步 |
| 日志 | 标准库 log/slog | 结构化日志 |
| 健康检查 | 定时探测 + 失败熔断 | 自研策略 |

## 五、配置示例（config.yaml 草案）

```yaml
server:
  port: 8787

upstreams:
  - name: vllm-local
    type: openai           # openai | openai-compatible | anthropic
    base_url: http://192.168.1.10:3001/v1
    api_key: ${VLLM_KEY}
    priority: 1            # 主备: 数字小=主
    weight: 100
    models:                # 可服务模型
      - qwen3.6:35b
    health_check:
      path: /models
      interval: 30s
      timeout: 5s

  - name: opencode-go
    type: openai
    base_url: https://opencode.ai/v1
    api_key: ${OPENCODE_KEY}
    priority: 2
    weight: 50
    quota:                 # 配额余量分流
      rolling: 5h          # 每5小时滚动限额
      weekly: 7d
      monthly: 30d
    models:
      - gpt-5
      - claude-sonnet

routing:
  default_strategy: quota   # quota | latency | primary_backup | weighted
  rules:
    - model: "qwen*"
      upstreams: [vllm-local]
      strategy: primary_backup
    - model: "gpt-5"
      upstreams: [opencode-go]
      strategy: quota
```

## 六、分阶段 Todo

### Phase 0 — 项目骨架 ✅
- [ ] `go mod init` 建项目（/data/codes/xuanji/）
- [ ] 引入 openai-go、yaml.v3
- [ ] config.yaml 解析 + 校验
- [ ] HTTP 服务启动（:8787），健康检查 `/healthz`
- **验收**：`curl /healthz` 返回 200

### Phase 1 — OpenAI 协议代理（单上游）
- [ ] `POST /v1/chat/completions` 非流式转发
- [ ] `POST /v1/chat/completions` 流式 SSE 透传
- [ ] 上游错误映射（429/5xx → 标准 OpenAI 错误格式）
- **验收**：Hermes 配置 base_url 指向网关，能正常对话（含流式）

### Phase 2 — 多上游 + 路由
- [ ] 上游组管理（增删改查内存态）
- [ ] 模型 → 上游组映射
- [ ] 健康检查定时器（失败摘除，恢复加回）
- **验收**：配置两个上游，模型路由到正确上游

### Phase 3 — 分流策略
- [ ] 主备策略（priority 排序 + 故障切换）
- [ ] 加权策略（weight 随机）
- [ ] 延迟策略（滑动窗口测速 + 最低延迟优先）
- [ ] 配额余量策略（上游上报/探测余量 + 按余量比例分配）
- [ ] 失败重试（上游 429/5xx → 自动换下一个上游）
- **验收**：拔掉主上游，请求自动切备用；配额耗尽的上游被跳过

### Phase 4 — Claude 协议兼容
- [ ] `POST /v1/messages` 入口（Anthropic 格式）
- [ ] OpenAI → Anthropic 请求体转换（system/messages/tools/max_tokens）
- [ ] Anthropic SSE → OpenAI SSE 转换（或直通 Anthropic 格式）
- [ ] Claude Code 直连网关验证
- **验收**：Claude Code 配 base_url 指向网关，能正常对话

### Phase 5 — 全类型覆盖 ✅（2026-08-01 完成）
- [x] embeddings 转发（已随 ollama 上游做掉）
- [x] images（文生图）转发 — internal/proxy/media.go `ImageGenerations`（真实上游 agnes 实测通过）
- [x] audio speech（TTS）转发 — `AudioSpeech`
- [x] audio transcriptions（STT）转发 — `AudioTranscriptions`
- [x] **mimo TTS 桥接**：mimo-v2.5-tts 系列走 `/v1/chat/completions` + audio 字段（标准 OpenAI `/v1/audio/speech` 端点 mimo 不支持），`api-key` 鉴权、voice 映射、base64 解码（internal/proxy/mimo_tts_test.go，5 测试）
- [x] main.go 注册三个路由（67 测试全绿）
- [x] **验收完成**：images(agnes 真实出图) + TTS(mimo 真实出 mp3) curl 实测通过；STT 暂无真实上游（mimo-v2.5-asr 可后续接入），mock 验证通过
### Phase 6 — 管理界面（✅ 6-A 完成 2026-08-01 | ✅ 6-B SQLite 持久化 2026-08-01 | ✅ 6-C 重试机制 2026-08-01 | ✅ 6-D 配置数据库化 2026-08-01）
- [x] admin JSON API（internal/admin）
- [x] amis 管理界面（web/admin.html，百度 amis 6.3.0 低代码 JSON schema）
- [x] SQLite 持久化（internal/store：Store + Recorder 异步批量写入，4 Handler 埋点）
- [x] admin metrics API（/admin/metrics/summary /upstreams /hourly）
- [x] 前端「请求统计」Tab（汇总卡片 + 每上游表格 + 24h 趋势图）
- [x] 重试机制增强（可配置状态码/关键词、maxRetries、跨上游重试）
- [x] 系统设置 Tab（重试策略展示 + 重载配置按钮）
- [x] 配置数据库化（UpstreamRow/RoutingRuleRow 表 + CRUD API + 热重载）
- [x] 前端 CRUD（上游/路由规则增删改按钮 + 弹窗表单）

### Phase 7 — 新功能待办池（2026-08-01 定，按序执行）

> 由用户确认：除 **prompt 缓存**（见下文"明确不做"）外，以下功能先入池，等 Phase 5 实测完成后**一个一个来**。
> 执行顺序即下方编号（1→2→3→4→5），每项完成并实测后回 PLAN.md 勾选。

#### 7.1 时间策略（time_based，夜间优惠）⭐ 最易落地
- **动机**：部分提供商夜间有更大优惠，时间段内优先使用
- **优先级**：仍在 tier 之后（tier 成本铁律不变），即 tier 分组后、同 tier 内排序前生效
- **配置扩展**（config.yaml）：
  ```yaml
  upstreams:
    - name: 硅基流动
      time_rules:                    # 新增字段
        - days: [0,1,2,3,4,5,6]     # 0=周日, 6=周六
          start: "23:00"
          end: "07:00"
          boost: 3                   # 该时段优先级提升 N 级（等效 priority 临时减 N）
  ```
- **实现**：config 加 `time_rules` 结构体；`proxy.selectCandidates` 在 tier 分组后、同 tier 排序前插入 `timeInRange` 计算 → `effectivePriority = priority - boost`，同 tier 内按 effectivePriority 升序
- **改动量**：~60 行 Go（config 结构体 + timeInRange 函数 + selectCandidates 扩展），不影响现有策略

#### 7.2 Upstream 动态增删（热更新）⭐⭐
- **核心矛盾**：Router 和 Health Checker 基于 `*config.Config` 静态指针构建，热更新需可刷新
- **方案**：`internal/config/holder.go` 新增 `ConfigHolder`（`atomic.Pointer[Config]`），`Load()` 重载 + `Get()` 原子读；Router/Health Checker 改为每次请求从 Holder 读；Health Checker loop 增量：新 upstream 自动启动探测 goroutine，移除的停止探测 + 优雅退出
- **触发**：`POST /admin/reload` + `SIGHUP` 双路
- **注意**：在途请求用旧配置、新请求用新配置（预期平滑切换）；删 upstream 时正在进行的流式请求不受影响
- **改动量**：~200 行，2 天

#### 7.3 统计 + 管理 API（metrics 数据层）⭐⭐
- **动机**：判断哪个上游可用性更好、使用量更高
- **数据层**：新增 `internal/metrics` 包，内存环形缓冲区 + 可选 SQLite 持久化
  ```go
  type Metrics struct { mu sync.RWMutex; buckets map[string]*UpstreamStats }
  type UpstreamStats struct {
      Requests/Successes/Failures int64
      TotalTokens int64
      AvgLatency, LatencyP50/P95 time.Duration
      LastChecked time.Time
      HourlyHistory [24]HourBucket  // 滑动窗口
  }
  ```
- **采集点**：`proxy.forwardOnce` 成功时 gjson 解析 `usage.total_tokens`；失败记 Failures；health check 结果也聚合
- **API 端点**：
  ```
  GET /admin/metrics           → 实时聚合（JSON）
  GET /admin/metrics/upstreams → 按上游展示
  GET /healthz/live            → 详细状态（各上游延时、状态）
  ```
- **前端**：先出 JSON API 用 curl 看，页面等 7.5 一起

#### 7.4 可用性统计（随 health check 做）⭐
- **现状缺陷**：health check 只记"当前状态"，不存历史可用率；转发的 MarkFailure 不聚合
- **扩展**：health.Checker 加 `UptimeTracker`：
  ```go
  type UptimeTracker struct {
      TotalChecks, SuccessChecks int64
      HourlyRate [24]float64   // 每小时可用率
      DailyRate  [7]float64    // 每天可用率
  }
  ```
- `checkOnce` 成功/失败都记入；`proxy.MarkFailure` 也记入 → `/admin/metrics` 展示"过去 24h 可用率 / 7 天可用率"
- **无需额外定时器**（health check 每 30s 已跑）
- **改动量**：~80 行，半天

#### 7.5 统计 Dashboard（Web 管理界面）⭐（依赖 7.3/7.4 数据）
- 简单 Web 仪表盘：上游状态（三态 + 延迟）、流量统计（请求/成功率/token）、可用率趋势
- 复用 7.3/7.4 的 JSON API，前端 Vue 3 或静态页面
- 原 Phase 6 的"动态配置热更新"已拆到 7.2，此阶段只做展示

#### 明确不做：Prompt 缓存复用 🚫
- **结论**：上游（vLLM/OpenRouter/OpenCode Go/硅基）服务端已有自动 prompt caching，网关层缓存只带来"首 token 延迟优化"而非成本节省；且流式场景实现复杂（缓存只能加速首 token）
- **状态**：用户拍板不入池，如未来需要再议

## 七、关键设计决策

1. **协议转换放网关边界**：内部统一用 OpenAI 协议结构，Anthropic 只做入口/出口转换，避免两套内部表示。
2. **流式用纯透传**：不缓存完整响应，用 io.Copy 边收边发，延迟最低；转换协议时才需要 buffer 解析。
3. **配额余量数据来源**：OpenCode Go 类上游无 API 查询时，用页面抓取（已有 opencode_usage_monitor.sh 经验）或本地估算表。
4. **熔断三态**：healthy / degraded（连续N次失败）/ dead（超时摘除），自动恢复带 backoff。
5. **成本层级 tier**：路由排序先按 tier（free < subscription < payg，免费优先、按量最后），同层内按 priority。上游未配置 tier 视为 payg（安全默认）。
6. **模型名透传**：网关不硬编码模型能力表，按上游 `models` 声明匹配，未匹配的模型走默认组或 404。

## 八、风险与对策

| 风险 | 对策 |
|------|------|
| Claude 工具调用转换复杂 | Phase 4 先保对话/流式，工具调用单列子任务 |
| 配额数据拿不到 | 先做"手动配置余量 + 请求计数本地估算" |
| 流式透传丢字段 | 用 openai-go 的 streaming 原生产品解析再重组 |
| oneapi 兼容性差的坑重演 | 每个 Phase 用 curl 全字段验证（tools/multimodal/logprobs） |
