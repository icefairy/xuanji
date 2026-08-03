# 别再折腾 CC Switch 和 OneAPI 了：18MB 单文件网关，9 家 AI 上游自动分流，挂了自动换，我的 Agent 半夜跑通宵一次没断

> Go · OpenAI/Anthropic 双协议 · 成本优先路由 · 自动故障切换
> 开源地址在文末。

## 先聊两句 CC Switch

最近 CC Switch 很火——桌面应用，一键在 Claude Code / Codex / Gemini CLI 之间切换 API 配置。确实解决了"手动改 JSON"的痛点。

但用久了你会发现问题：

1. **还是手动切**：点一下切换配置，很多工具要重启终端才生效；
2. **每台机器装一个**：桌面客户端，换电脑就得重配；
3. **只管切换，不管调度**：你切到渠道 A，A 挂了你还得手动切回 B。

而 OneAPI 呢？服务端网关，功能全——但重：Go 单二进制却塞满了计费、用户、兑换码等运营功能，Docker 镜像 ~200MB 还常要配 MySQL/Redis，部署维护成本高，个人用属于杀鸡用牛刀。

## 我的解法：璇玑 Xuanji

一句话：**Go 单二进制的 AI 协议网关**。给 Hermes、Claude Code、OpenCode 等工具一个统一入口，汇聚多家上游，**按成本与健康状态自动分流**。

它的核心逻辑跟 CC Switch 完全不同：

| 维度 | CC Switch | 璇玑 Xuanji |
|------|-----------|-------------|
| 形态 | 桌面客户端，每台机器装一个 | 服务端网关，一个地址全设备共用 |
| 切换方式 | 手动点选，大多要重启终端 | **无需切换**：工具永远指向网关，网关自动选上游 |
| 故障转移 | 手动：发现失败后人工切 | **自动**：失败切同层下一个，整层失败升层级 |
| 多机共享 | 各机器独立配置 | 多实例共享同一数据库 |
| 配置管理 | 桌面 GUI | Web 管理页，CRUD 即热重载 |

**核心论点一句话：CC Switch 是"多套配置之间的切换器"，璇玑是"所有配置之上的调度器"。** 用璇玑之后不需要"切换"这个动作——Claude Code 配一次 `ANTHROPIC_BASE_URL` 指向网关，之后渠道变更都在网关后台完成，客户端无感。

## 自动分流的威力：半夜跑通宵一次没断

我的 Agent 任务很多是夜间批量跑的。以前用直连渠道，某家上游半夜抽风，第二天起来一看——昨晚任务全断了，白跑一晚。

接上璇玑之后：

```
免费层（free）→ 订阅层（subscription）→ 付费层（payg）
```

有免费先走免费，免费挂了升订阅，订阅再挂才动付费。配合权重调度：同层级内按权重分配流量，避免单点过载。

实测近 7 天 1943 个请求，2.3 亿 tokens，成功率 99.3%：

![统计总览：近 7 天 1943 请求、99.3% 成功率、2.3 亿 tokens](../screenshots/dashboard.png)

任何一个上游挂了，请求自动切到下一个——**工作流从不中断**，这就是"自动分流"和"手动切换"的本质区别。

## 双协议原生：OpenAI + Anthropic 都原生支持

大多数网关只做 OpenAI 协议，Anthropic 要靠"翻译"层。璇玑是**双协议原生实现**，流式 SSE 逐行透传、工具调用多模态字段完整保留。

Claude Code 直接连：

```bash
export ANTHROPIC_BASE_URL=http://你的服务器:3002
export ANTHROPIC_AUTH_TOKEN=网关发的Key
```

之后上游怎么换、渠道怎么调，客户端完全无感。

![上游管理：13 个上游，状态/权重/延迟/模型数一目了然](../screenshots/upstreams.png)

![路由规则：模型支持通配符，每个模型可绑定多个上游](../screenshots/routing.png)

## 零配置数据库化：改配置不重启

所有配置存 SQLite，页面 CRUD **即改即热重载**。管理 API 带独立鉴权 key，AI 助手可以免登录动态改配置——让 Agent 自己调路由、自己加渠道。

![API Key 管理：创建下游 Key，控制访问权限](../screenshots/api-keys.png)

**多 Key 统计**：给每个 AI 程序分配不同 Key，管理页按 Key 看用量，知道每个程序花了多少。

## 极致的轻

- 单二进制 13MB，解压即用；SQLite 纯 Go 无 CGO；
- `docker compose up -d` 一行拉起，镜像 18MB；
- 144 个自动化测试全绿。

```bash
docker compose up -d --build
# 或直接跑二进制
./xuanji-server --port 8787 --db /path/to/xuanji.db
```

## 什么时候选谁

- 你在 **单机、单工具** 上频繁切渠道 → CC Switch 够用；
- 你有多台机器、多个 Agent、**要自动调度和故障转移** → 璇玑；
- 你要做多用户计费分发 → OneAPI 那一类；
- 你**个人自用，不想伺候 MySQL/Redis 和一堆运营功能** → 璇玑。

## 开源地址

- GitHub：https://github.com/icefairy/xuanji
- Gitee 镜像（国内快）：https://gitee.com/icefairy/xuanji-gateway

Star 支持一下，有问题欢迎提 Issue。

---

*个人业余作品，没有计费功能，短期内也不打算加——个人自用，够了。*
