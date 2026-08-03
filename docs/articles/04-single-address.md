# 震惊：一个 18MB 的 Go 二进制，让我的 Claude Code / OpenCode / Hermes 全部指向同一个地址，上游随便挂，工作流从不中断

> Go · OpenAI/Anthropic 双协议 · 成本优先路由 · 自动故障切换
> 开源地址在文末。

## 先看结果

近 7 天，我的所有 AI 工具——Claude Code、OpenCode、Hermes、玄盘——全部指向同一个网关地址：

**1943 个请求，2.3 亿 tokens，请求成功率 99.3%。**

![统计总览：近 7 天 1943 请求、99.3% 成功率、2.3 亿 tokens](../screenshots/dashboard.png)

这一个地址背后是 13 个上游渠道：硅基流动、商汤、DeepSeek 系中转、自建 vLLM、OpenRouter……任何一个挂了，请求自动切下一个，**工作流从不中断**。

## 这是什么

「璇玑 Xuanji」——Go 单二进制的 AI 协议网关。为 Hermes、Claude Code、OpenCode 等 AI 工具提供一个统一入口，汇聚多家上游，按成本与健康状态自动分流。

名字取自北斗第一星——古代天文仪器的轴心枢纽，汇聚万向、运转分流。

## 硬核点一：双协议原生，不是"翻译"是"透传"

大多数网关只做 OpenAI 协议，Anthropic 协议要靠一层"翻译"——翻译就必然有损：流式丢行、工具调用字段丢失、多模态 content 结构被拆坏。

璇玑是 **双协议原生实现**：

| 协议 | 端点 | 说明 |
|------|------|------|
| **OpenAI** | `POST /v1/chat/completions` | 聊天补全（流式/非流式） |
| | `POST /v1/embeddings` | 文本嵌入 |
| | `POST /v1/images/generations` | 图片生成 |
| | `POST /v1/rerank` | 重排 |
| | `POST /v1/audio/speech` | 语音合成 |
| | `POST /v1/audio/transcriptions` | 语音识别 |
| | `GET /v1/models` | 模型列表 |
| **Anthropic** | `POST /v1/messages` | Claude 消息（流式事件转换） |

- **流式 SSE 逐行透传**，不缓冲、不截断，首 token 延迟最低；
- **工具调用、多模态字段**完整保留；
- Claude Code 直接连：`ANTHROPIC_BASE_URL` 指向网关，`ANTHROPIC_AUTH_TOKEN` 用网关发的 Key。

## 硬核点二：成本优先的三级路由

每个模型可绑定多个上游，网关按 **计费层级** 调度：

```
免费层（free）→ 订阅层（subscription）→ 付费层（payg）
```

**有免费先走免费，免费挂了升订阅，订阅再挂才动付费。** 同层级内按权重调度，避免单点过载。

## 硬核点三：高可用，上游挂了不中断

- **自动分流**：按计费层级、权重、健康度自动选最优通道；
- **自动重试**：上游失败自动切同层下一个，整层失败升层级——**不会因为任何一个上游故障导致请求中断**；
- **健康检查**：每上游独立探测，healthy / degraded / dead 三态；
- **快速失败熔断**：失败上游自动进冷却名单，后台定时探测自动恢复；
- **客户端断连保护**：客户端断连不误拉黑上游（踩过的坑：6 个上游被误拉黑，已修复并加回归测试）。

![上游管理：13 个上游，状态/权重/延迟/模型数一目了然](../screenshots/upstreams.png)

![路由规则：模型支持通配符，每个模型可绑定多个上游](../screenshots/routing.png)

## 硬核点四：零配置文件，全数据库化

所有配置存 SQLite，页面 CRUD **即改即热重载**，不用重启、不用改 YAML。管理 API 带独立鉴权 key，AI 助手可免登录动态改配置——让 Agent 自己调路由、自己加渠道。

![API Key 管理：创建下游 Key，控制访问权限](../screenshots/api-keys.png)

**多 Key 统计**：给每个 AI 程序分配不同 Key，按 Key 看用量——哪个程序用得多、token 花在哪，清清楚楚。

## 硬核点五：极致的轻

- **单二进制**：Go 静态编译，13MB，解压即用；
- **SQLite 存储**：`modernc.org/sqlite` 纯 Go 实现，无 CGO，WAL 模式 + 内存页缓存；
- **部署**：`docker compose up -d` 一行拉起，镜像 18MB；
- **无框架**：标准库 `net/http` 增强路由。

```bash
docker compose up -d --build
# 或直接跑二进制
./xuanji-server --port 8787 --db /path/to/xuanji.db
```

## 质量

**144 个自动化测试全绿**，覆盖双协议转换、路由调度、熔断恢复、上下文取消等关键路径。生产环境持续运行验证。

## 技术栈

- **语言**：Go 1.26+（标准库 `net/http` 增强路由，无框架）
- **存储**：SQLite（`modernc.org/sqlite` 纯 Go 无 CGO），WAL 模式 + 内存页缓存
- **上游 SDK**：`openai-go`（协议严格）
- **前端**：Vue 2 CDN + 原生 HTML（无构建工具，离线可用）

## 开源地址

- GitHub：https://github.com/icefairy/xuanji
- Gitee 镜像（国内快）：https://gitee.com/icefairy/xuanji-gateway

Star 支持一下，有问题欢迎提 Issue。

---

*个人业余作品，技术栈选型偏"知白守黑"——不用微服务、不用 K8s，单机单二进制解决 90% 的问题。没有计费功能，短期内也不打算加。适合：个人开发者、小团队、自建 AI 基础设施的人。*
