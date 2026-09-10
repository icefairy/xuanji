# 璇玑 Xuanji —— AI 协议网关（快速介入指南）

> 生成于 2026-09-10，基于当前代码库实测探测。**本文件是权威上下文**；根目录 CLAUDE.md 与 PLAN.md 是早期规划，已完成/过期的内容以本文件为准。

## 项目是什么

Go 单二进制 + SQLite 的轻量 AI 网关：对外暴露 **OpenAI 协议** 与 **Anthropic 协议** 双入口，上游为多个 OpenAI 兼容端点（vLLM、硅基流动、商汤、DeepSeek 中转、OpenRouter、Ollama、mimo TTS 等），按**计费层级 + 权重 + 优惠时段 + 延迟**自动分流、故障切换、熔断恢复。

定位：个人/小团队自用的一站式网关（one-api / LiteLLM / CC Switch 的轻量替代）。Apache 2.0。

- 仓库：`github.com/icefairy/xuanji`（Gitee 镜像 `gitee.com/icefairy/xuanji-gateway`，自建 Gitea `192.168.1.10:3000/icefairy/xuanji-gateway`）
- 最新 tag：v1.7.0（release notes 模板在 `scripts/release_notes_*.md`）
- 测试：`go build ./... && go test ./...`，当时 ~189 个测试全绿

## 运行中的生产实例（勿随意动）

- **主网关**：`/data/xuanji/xuanji-server --port 3002 --db /ssd/migrated/xuanji/xuanji.db`（supervisor 管理，worker `xuanji`）
- **备用网关**：`/data/codes/xuanji-fallback/xuanji-fallback -port 3004`（自研备用 18MB，替代曾经的 oneapi）
- 数据库已从本目录迁移到 `/data/migrated/xuanji/`（本目录下 `xuanji.db` 是 0 字节占位，勿当真实库）
- 本地测试 mock 上游跑在 `/tmp/xuanji-test/`（mock_upstream.js / mock_gemini.js，node 常驻）

## 技术栈

- Go 1.26+（本机），标准库 `net/http`（1.22 增强路由）+ `log/slog`，**无 Web 框架**
- SQLite：`modernc.org/sqlite`（纯 Go 无 CGO），WAL 模式；**全部配置存 SQLite**（首次启动自动建库 + 写入默认配置），config.yaml 仅作样例/兼容
- gjson + sjson（JSON 操作）、tiktoken-go（非流式 token 估算）、yaml.v3、bcrypt+自实现 JWT（HMAC-SHA256，零依赖）
- 前端：Vue 2 CDN + 原生 HTML，`go:embed` 嵌入二进制，无构建工具

## 代码结构

```
cmd/server/main.go        入口：DB 打开 → 组件装配 → buildServeMux 路由 → 优雅退出
cmd/server/web/           管理界面（admin_vue.html + vue/ 第三方静态资源），go:embed 进二进制
internal/proxy/            核心代理：messages(chat)、completions、media(images/audio)、
                           thinking(思考强度归一化)、reasoning_cache/reasoning_content、
                           prompt_cache、fastfail(熔断)、tokenlimit、request_override、
                           strip_fields(字段剥离)、video(视频透传)、maxtokens、imageurl、
                           errordetail、role、tokenizer、arrears(欠费)
internal/admin/            管理 JSON API（JWT 鉴权 + 管理 API key 双轨）
internal/store/             SQLite 数据层（最大文件 ~2700 行）：upstreams/routing_rules/
                           discounts/api_tokens/request_logs/metrics/probes/backups/
                           groups(配额矩阵)/config 表，CRUD 即热重载
internal/auth/             API Key（Bearer / x-api-key 双风格）+ JWT 签发校验
internal/router/           模型 → 上游组路由
internal/health/           每上游独立健康探测 + 三态（healthy/degraded/dead）
internal/quota/            配额策略服务（组×模型 白名单 + 模型级配额，key 可覆盖）
internal/anthropic/        对外 Anthropic 协议（/v1/messages）转换
internal/gemini/           Gemini 上游协议转换
internal/ollama/           OLLAMA 上游协议转换
scripts/                   build.sh(编译注入 buildDate) / deploy.sh(编译+supervisor 部署) /
                           create_github_release.sh / create_gitee_release.sh /
                           nightly_push.sh(定时推远端) / pi-autofix.sh(定时自愈) /
                           eval_models.py 评测工具(评分 57 题)
tools/                     模型评测脚本（eval_models.py / run / report）
docs/                      使用说明.md(小白图文)、CHANGELOG、screenshots、usage-guide
dist/                      三平台发布二进制（gzip）
config.example.yaml        YAML 样例（演示 ${ENV} 占位符）
```

## 核心行为（必须理解再做改动）

1. **协议**：内部统一 OpenAI 结构；Anthropic /gemini /ollama 只在入口/出口转换；SSE 流式一般**逐行透传不解析**，仅转换时解析
2. **路由优先级**（写死在 proxy）：`tier（免费 free < 包月 subscription < 按量payg）→ weight 高优先 → 优惠时段 → 健康延迟低优先`；同层失败逐个尝试，整层失败升级下一计费层，全失败返回 502
3. **健康检查**：按上游 `kind`（chat/emb/rerank/tts/asr/image）选探测端点，占位模型名 + 无凭证请求，零推理消耗；**2xx 与全部 4xx 算"服务在线"，仅 5xx/超时/断连判 dead**——不要误判纯 TTS/embedding 上游；客户端断连（context.Canceled）不拉黑上游
4. **思考强度归一化**：客户端传标准 `reasoning_effort: none|low|medium|high`，网关按模型族自动转换（DeepSeek=原生、商汤=output_config.effort、Kimi/GLM=thinking.type 开关、Qwen=enable_thinking + 分档 budget、OpenAI=透传）；无 reasoning_effort 时零开销原样透传
5. **最佳思考等级**：DB 预置推荐值（deepseek-v4-flash=high 等），系统设置"自动设置"/"强制覆盖"两开关
6. **reasoning_content 回传**：仅对 DeepSeek 模型注入指纹；缓存双写(内存+SQLite, 保留7天)，自动学习模型 token 上限并 clamp
7. **配额**：internal/quota 组×模型矩阵；数据库 `admin.groups` 表；API Key 可设 policy 覆盖；有 management 页「分组配额」
8. **配置热重载**：admin CRUD → 写 DB → 触发 reload（`reloadConfig`），在途请求用旧配置，新请求用新
9. **User-Agent**：上游默认 UA = config.DefaultUpstreamUserAgent（可系统设置改），放 Header

## 鉴权体系

- **管理页**：`/` 登录（默认 admin / xuanji123，**生产已改**），JWT `Bearer` 携带，`/admin/*` 全走 `adminAuth`（除 /admin/login）
- **管理 API key**：config 表 `admin.api_key`，AI 助手免登录改配置用（`/api/admin/*` 需单独 header）——见 main.go `adminKeyAuth`
- **下游客户端**：签发 API Key（auth 包），支持 `Authorization: Bearer <key>` 或 `x-api-key: <key>` 双风格请求
- **磁盘上的 web/ 目录 vs 嵌入资源**：磁盘 `web/` 存在则优先（开发热改）；发布/部署后**不要**在磁盘放 web/，会遮蔽嵌入资源（详见 deploy.sh 注释）

## 数据库注意

- 库表（`internal/store/store.go`，唯一数据访问层，含自动建列 `ensureColumn`——加字段时在 store.go 里做，旧库自动 ALTER）：upstreams、routing_rules、discounts、api_tokens、groups + group_model_quota（配额矩阵）、client_profiles、effort_config（思考等级）、model_prices、model_token_limits（token 上限自动学习）、reasoning_cache、request_log、daily_stats、health_probe_log、upstream_model_arrears、video_jobs、users、config
- 配置文件化后 **config.yaml 不再管运行**（仅样例）；任务标志 `-db`、`-port` 控制运行参数

## Git 工作流

- 分支：`main`（主开发）、`public`（对外公开版）、`backup-public`（备份）
- 远端 3 个：github（git@ssh）、gitee（https）、gitea（内网 http，**URL 含凭据，勿提交到任何文档**）
- **daily_push.sh**（cron 每日 + pi-autofix.sh 自愈）：auto-commit 未提交变更（`chore: auto-commit pending changes at %s`）
- 发版：先 build.sh 编译三平台 → 按 xuanji-release skill 走（写 release notes → 推 tag → GitHub/Gitee Release）
- **push 前必看**：`git status` 工作区是否干净，.env / config.yaml / *.db / 二进制不得入版本

## 常用命令

```bash
go build ./... && go test ./...          # 全量构建+测试（改动后必跑）
./scripts/build.sh vX.Y.Z              # 编译 xuanji-server（注入 buildDate）
./deploy.sh 3002                       # 一键部署到 supervisor（生产用，别在本地瞎跑）
./xuanji-server --port 8787 --db ./xuanji.db  # 本地起服务
curl http://127.0.0.1:3002/healthz    # 探活
```

## 踩过的坑（写代码时避免）

- 客户端断连时 `context.Canceled` 曾导致上游被误拉黑——判死前必须区分"客户端断"与"上游断"
- token 计数：流式用 `stream_options.include_usage` 逐 chunk 解析；非流式回退 tiktoken 估算（内部有 cl100k_base.tiktoken 内置文件）
- image_url 曾强制拍平嵌套，后改**上游可配开关，默认保留 OpenAI 标准嵌套格式**（反转了，注意别又拍平）
- `reasoning_content` 注入只能对 DeepSeek 模型，防伤其他模型（严格上游 400 校验测试别删）
- 透传的 4xx（如 413）不算上游健康失败——透传状态码决定是否熔断
- supervisor 部署用 `--port` / `-db` 参数，端口改在 supervisor 配置而非 config