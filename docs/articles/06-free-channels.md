# 我把手头 9 个 AI 渠道（含免费白嫖的）塞进一个网关，全自动调度——朋友看完直接要走了源码

> Go · OpenAI/Anthropic 双协议 · 成本优先路由 · 自动故障切换
> 开源地址在文末。

## 我的"白嫖"清单

做 AI 这一年多，我攒了一堆渠道，很多是免费或极低成本的：

- **硅基流动**：注册送的额度，够跑一阵；
- **商汤**：体验金，时不时有活动；
- **各种中转站**：有些模型免费试用；
- **自建 vLLM**：自己机器上部署的开源模型，电费而已；
- **DeepSeek 官方/中转**：便宜，但不是免费；
- **OpenRouter**：有些免费模型。

问题是——**渠道多了反而难用**：每个渠道 Key 不一样、协议不完全一样、免费额度各有各的限制。以前的状态是：

1. 工具里来回切 Key，改一个配置动好几个文件；
2. 免费额度在哪、剩多少，全靠脑子记；
3. 某家免费渠道抽风，请求全挂，你还不知道去哪看日志；
4. 付费渠道和免费渠道混在一起，没有调度逻辑——**白嫖额度永远用不完，付费账单倒是先爆了**。

## 解法：一个网关，全自动调度

「璇玑 Xuanji」——Go 单二进制的 AI 协议网关。为 Hermes、Claude Code、OpenCode 等工具提供统一入口，汇聚多家上游，按成本与健康状态自动分流。

核心逻辑：

```
免费层（free）→ 订阅层（subscription）→ 付费层（payg）
```

**有免费先走免费，免费挂了升订阅，订阅再挂才动付费。**

免费额度用完？自动切付费兜底，不影响工作流。免费渠道抽风？自动换下一个，客户端无感。

实测近 7 天 1943 个请求，2.3 亿 tokens，成功率 99.3%——相当一部分流量吃的是免费额度：

![统计总览：近 7 天 1943 请求、99.3% 成功率、2.3 亿 tokens](../screenshots/dashboard.png)

**朋友看完效果直接要走了源码。** 开源，GitHub 上就有。

## 13 个上游怎么管

![上游管理：13 个上游，状态/权重/延迟/模型数一目了然](../screenshots/upstreams.png)

每个上游独立健康检查：healthy / degraded / dead 三态，挂了的自动进冷却名单，后台定时探测恢复。快速失败熔断，失败上游秒级拉黑。

## 路由规则：白嫖渠道绑满

![路由规则：模型支持通配符，每个模型可绑定多个上游](../screenshots/routing.png)

每个模型可以绑多个上游——免费渠道绑在前，付费渠道兜底。模型支持通配符，配一次管一片。

## 双协议原生：OpenAI + Anthropic

璇玑是 **双协议原生实现**：流式 SSE 逐行透传、工具调用多模态字段完整保留。Claude Code 直接连：

```bash
export ANTHROPIC_BASE_URL=http://你的服务器:3002
export ANTHROPIC_AUTH_TOKEN=网关发的Key
```

| 协议 | 端点 |
|------|------|
| OpenAI | `/v1/chat/completions`、`/v1/embeddings`、`/v1/images/generations`、`/v1/rerank`、`/v1/audio/*`、`/v1/models` |
| Anthropic | `/v1/messages` |

## 多 Key 统计：白嫖也要算清楚

给每个 AI 程序分配不同 Key，管理页按 Key 看用量——哪个程序把免费额度吃完了、哪个程序成功率低，一目了然。**免费额度用在哪了，每一分都清楚。**

![API Key 管理：创建下游 Key，控制访问权限](../screenshots/api-keys.png)

## 零配置：改配置不重启

所有配置存 SQLite，页面 CRUD **即改即热重载**。管理 API 带独立鉴权 key，AI 助手可免登录动态改配置。

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
