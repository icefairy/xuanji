# 程序员被 AI 上游限流逼疯后，写了个 18MB 的"路由器"：免费渠道优先，付费兜底，Claude Code 从此不用切 Key

> Go · OpenAI/Anthropic 双协议 · 成本优先路由 · 自动故障切换
> 开源地址在文末。

## 被限流逼疯的那一晚

凌晨两点，我盯着终端里密密麻麻的 `429 rate limit exceeded`，心态崩了。

那天我接了 40 个 Agent 任务，跑批处理。手上有五六个渠道的 Key——硅基流动有免费额度、商汤送了体验金、DeepSeek 中转充了钱、还有个 vLLM 自建服务。本来应该"谁有额度谁上"，但事实是：

- 所有 Agent 都在打同一个渠道 → 限流；
- 我手动切到另一个 → 三分钟后它也开始 429；
- 免费渠道永远吃不到流量，付费账单倒是先爆了；
- Claude Code 的配置在 `~/.claude.json` 里改来改去，一个项目一套配，乱了。

那天晚上我把所有 Agent 任务改成了手动排队、挨个换 Key 重跑——**效率直接打三折**。

## 路由器思路

天亮之后我决定写个东西。思路其实特别朴素——**像路由器一样转发流量**：

1. 所有 AI 工具只认一个地址（网关）；
2. 网关背后挂 N 个上游渠道；
3. 来一个请求，网关看模型、看渠道健康度、看计费层级，**自动选最合适的通道**；
4. 上游挂了？自动切下一个，客户端无感。

这就是「璇玑 Xuanji」——Go 单二进制的 AI 协议网关。

## 路由逻辑：免费优先，付费兜底

每个模型可以绑定多个上游，网关按 **计费层级** 调度：

```
免费层（free）→ 订阅层（subscription）→ 付费层（payg）
```

规则：**有免费先走免费，免费挂了升订阅，订阅再挂才动付费**。

同层级内按权重分配流量，高权重多发、低权重少发，避免单点过载。

实测近 7 天 1943 个请求，2.3 亿 tokens，成功率 99.3%——其中相当一部分流量吃的是免费额度：

![统计总览：近 7 天 1943 请求、99.3% 成功率、2.3 亿 tokens](../screenshots/dashboard.png)

**从此再也没见过批量 429。** 限流了自动换渠道，渠道死了自动升层级，我只需要在后台看着。

## 上游管理：状态一眼看清

![上游管理：13 个上游，状态/权重/延迟/模型数一目了然](../screenshots/upstreams.png)

每个上游独立健康检查：healthy / degraded / dead 三态，挂了的自动进冷却名单，后台定时探测恢复。快速失败熔断，失败上游秒级拉黑。

## 路由规则：一个模型多个通道

![路由规则：模型支持通配符，每个模型可绑定多个上游](../screenshots/routing.png)

模型支持通配符，每个模型可绑多个上游。Claude Code 配一次就完事：

```bash
export ANTHROPIC_BASE_URL=http://你的服务器:3002
export ANTHROPIC_AUTH_TOKEN=网关发的Key
```

之后渠道怎么变，客户端永远不用动。

## 双协议原生：OpenAI + Anthropic

大多数网关只做 OpenAI 协议，Anthropic 要靠"翻译"层，翻译就必然有损。璇玑是**双协议原生实现**：流式 SSE 逐行透传、工具调用多模态字段完整保留。

| 协议 | 端点 |
|------|------|
| OpenAI | `/v1/chat/completions`、`/v1/embeddings`、`/v1/images/generations`、`/v1/rerank`、`/v1/audio/*`、`/v1/models` |
| Anthropic | `/v1/messages` |

## 多 Key 统计：知道谁在用、用了多少

给每个 AI 程序分配不同 API Key，管理页统计 Tab 按 Key 看用量——哪个 Agent 任务吃 token 最多、哪个程序成功率最低，一目了然。

![API Key 管理：创建下游 Key，控制访问权限](../screenshots/api-keys.png)

## 零配置：改配置不重启

所有配置存 SQLite，页面 CRUD **即改即热重载**，不用重启、不用改 YAML。管理 API 带独立鉴权 key，AI 助手可免登录动态改配置。

## 极致的轻

- 单二进制 13MB，SQLite 纯 Go 无 CGO；
- `docker compose up -d` 一行拉起，镜像 18MB；
- 144 个自动化测试全绿。

```bash
docker compose up -d --build
# 或直接跑二进制
./xuanji-server --port 8787 --db /path/to/xuanji.db
```

## 开源地址

- GitHub：https://github.com/icefairy/xuanji
- Gitee 镜像（国内快）：https://gitee.com/icefairy/xuanji-gateway

Star 支持一下，有问题欢迎提 Issue。

---

*个人业余作品，没有计费功能，短期内也不打算加——个人自用，够了。*
