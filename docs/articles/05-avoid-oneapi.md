# 自研 AI 网关避坑实录：别装 OneAPI 全家桶了，单文件跑起来，双协议互转，成本优先路由，这才是个人玩家该用的

> Go · OpenAI/Anthropic 双协议 · 成本优先路由 · 自动故障切换
> 开源地址在文末。

## 先说结论

个人玩 AI 网关，**别一上来就 OneAPI 全家桶**。它功能是全，但那是给"运营一个 AI 平台"准备的——多用户、计费、兑换码、配额……**个人自用，90% 的功能你用不上，却要为它背着整套运维负担。**

这篇说说我踩过的坑，和最后的解法。

## 坑一：OneAPI 太重

Go 单二进制但功能堆得重，Docker 镜像 ~200MB，为了跑全功能还得伺候 MySQL/Redis——个人服务器上这简直是灾难。

## 坑二：协议兼容差

很多网关只做 OpenAI 协议，Anthropic 协议要靠"翻译"层。翻译就必然有损：

- 流式 SSE 丢行；
- 工具调用字段丢失；
- 多模态 content 结构被拆坏。

Claude Code 连上去，`/v1/messages` 走不通，或者工具调用莫名其妙报错——排查到怀疑人生。

## 坑三：改配置要重启

改一个上游地址要重启服务，或者 API 调用链巨长，运维心智负担重。

## 坑四：没有成本路由

只管转发，不管"这个模型该走免费还是付费"。免费额度永远用不完，付费账单倒是先爆了。

## 我的解法：璇玑 Xuanji

一句话：**Go 单二进制的 AI 协议网关**。为 Hermes、Claude Code、OpenCode 等工具提供统一入口，汇聚多家上游，按成本与健康状态自动分流。

### 避坑点一：真的轻

- 单二进制 13MB，解压即用；
- SQLite 纯 Go 实现（`modernc.org/sqlite`），无 CGO，不需要 MySQL/Redis；
- `docker compose up -d` 一行拉起，镜像 18MB；
- 标准库 `net/http` 增强路由，无框架。

```bash
docker compose up -d --build
# 或直接跑二进制
./xuanji-server --port 8787 --db /path/to/xuanji.db
```

### 避坑点二：双协议原生

璇玑是 **双协议原生实现**，不是"翻译"：

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

- 流式 SSE 逐行透传，不缓冲、不截断；
- 工具调用、多模态字段完整保留；
- Claude Code 直接连：`ANTHROPIC_BASE_URL` 指向网关。

### 避坑点三：零配置热重载

所有配置存 SQLite，页面 CRUD **即改即热重载**，不用重启、不用改 YAML。管理 API 带独立鉴权 key，AI 助手可免登录动态改配置。

### 避坑点四：成本优先路由

每个模型可绑定多个上游，网关按 **计费层级** 调度：

```
免费层（free）→ 订阅层（subscription）→ 付费层（payg）
```

**有免费先走免费，免费挂了升订阅，订阅再挂才动付费。** 实测近 7 天 1943 个请求，2.3 亿 tokens，成功率 99.3%：

![统计总览：近 7 天 1943 请求、99.3% 成功率、2.3 亿 tokens](../screenshots/dashboard.png)

## 高可用：上游挂了不中断

- **自动分流**：按计费层级、权重、健康度自动选通道；
- **自动重试**：失败切同层下一个，整层失败升层级——**不会因为任何一个上游故障导致请求中断**；
- **健康检查**：每上游独立探测，healthy / degraded / dead 三态；
- **快速失败熔断**：失败上游自动进冷却名单，后台定时探测恢复。

![上游管理：13 个上游，状态/权重/延迟/模型数一目了然](../screenshots/upstreams.png)

![路由规则：模型支持通配符，每个模型可绑定多个上游](../screenshots/routing.png)

## 多 Key 统计：按程序看用量

给每个 AI 程序分配不同 Key，管理页统计 Tab 按 Key 看用量——哪个程序用得多、token 花在哪，一目了然。

![API Key 管理：创建下游 Key，控制访问权限](../screenshots/api-keys.png)

## 什么时候用 OneAPI，什么时候用璇玑

- **要做多用户计费分发、运营一个 AI 平台** → OneAPI 那一类；
- **个人自用、小团队，不想伺候 MySQL/Redis 和一堆运营功能** → 璇玑；
- **想清楚自己到底要什么** → 先看需求再选型，别被"全家桶"绑架。

## 开源地址

- GitHub：https://github.com/icefairy/xuanji
- Gitee 镜像（国内快）：https://gitee.com/icefairy/xuanji-gateway

Star 支持一下，有问题欢迎提 Issue。

---

*个人业余作品，没有计费功能，短期内也不打算加——定位就是个人/小团队自用。*
