# v1.2.0

## ⏱️ 新功能：时间线注入（让模型知道「过了多久」）

大模型没有时间感。你和它聊了三小时再回来接着问，它还以为刚刚才说过话——客户端把历史消息原样发过来，里面一个时间戳也没有。

璇玑现在可以在**最后一条 user 消息前**补上两行：

```
【时间】2026-09-10 14:05 周四（Asia/Shanghai）
【距上轮】3 小时 22 分钟
过了很久我又来了…
```

**默认关闭**，在「系统设置 → 时间线注入」一键开启。

### 为什么是「只改最后一条」

前面所有历史消息一个字节都不动，上游的**前缀缓存完全不受影响**——这是本功能能做到零成本的关键。若给每条消息都打时间戳，缓存会当场全废。

### 会话怎么识别（无需客户端配合）

网关是无状态的，只能靠指纹认人：

- `chainKey` = hash(API Key + 全部消息内容)，对长对话做前缀采样（首 4 条 + 末 3 条），保证跨轮哈希稳定
- `tailKey` = hash(最后一条消息)，区分「新的一轮」与「同一轮重发」——重试不会把间隔清零
- 客户端每轮会把 assistant 回复追加进历史，前后两轮不是前缀关系，所以用**多候选回退**（截掉尾部 1~3 条逐一试查）把两者接上

### 存储

内存 LRU（默认上限 10 万会话、TTL 30 天）+ SQLite 异步落库，**进程重启后「距上轮」依然连续**。只存哈希与时间戳，不落任何消息明文。

### 配置项

| 键 | 默认 | 说明 |
|---|---|---|
| `proxy.time_context_enabled` | `false` | 总开关 |
| `proxy.time_context_tz` | `Asia/Shanghai` | 时区（IANA 名） |
| `proxy.time_context_format` | `%Y-%m-%d %H:%M` | strftime 格式 |
| `proxy.time_context_template` | `【时间】{{datetime}} {{weekday}}（{{tz}}）` | 注入模板 |
| `proxy.time_context_min_gap` | `5` | 间隔小于 N 分钟不显示 |
| `proxy.time_context_include_gap` | `true` | 是否显示「距上轮」 |
| `proxy.time_context_max_sessions` | `100000` | 会话表上限 |
| `proxy.time_context_ttl_days` | `30` | 会话保留天数 |
| `proxy.time_context_persist` | `true` | 重启后是否保留 |

### 适用范围

`/v1/chat/completions`、`/v1/messages`（Anthropic）、Gemini 三个入口共用同一张会话表，切协议时时间线连续。纯工具回填轮次（content 只有 `tool_result`/`tool_use`）自动跳过。

### 已知边界

- 用户在客户端里编辑或删除历史消息 → 指纹变了，会被当成新会话，丢失一次间隔记忆（不会报错）
- 两个不同会话恰好共享完全相同的历史前缀 → 可能被合并（实际极少见）
- 间隔粒度取决于会话表 TTL，超过 30 天视为新会话
- 这是**近似**方案。如果你用 Hermes Agent 这类支持插件的客户端，[hermes-time-context](https://github.com/icefairy/hermes-time-context) 能做到精确到秒且不受上述限制——两者可共存，也可只开一头

---

## 其他改进

### 稳定性

- 客户端请求类 4xx（模型名不存在 / 输入超长 / rerank query 空串）不再计入上游故障，避免误拉黑健康上游
- 透传的 4xx（如 413）不计入上游健康失败
- 流式断连记 499 而非 502，口径更准确
- 修复 vision 兜底转发时未裁剪历史上下文的问题
- 转发前清理孤儿 tool 消息（缺 `tool_call_id`），规避 sglang 系严格校验 400

### 路由与协议

- 出站请求伪装成 pi agent（头部集合 + 顺序），避免上游把 `Go-http-client` 误判为脚本
- GLM-5.x 思考归一化：`none`/`off` 转为 `thinking.type=disabled`
- 竖线映射按顺序选取、日志记录真实模型名、旁路请求独立连接池
- 转发连接闲置超时 30s

### 管理界面

- 二级分组导航，菜单改为悬浮窗，触屏可再次点击收起，刷新保持位置
- 配置删除 API
- 「组配额」新增模型配额时三窗口全 0 不再静默丢弃
- 上游编辑表单超时字段回显修复（此前保存会误清零）
- 对话调试页补思考 token 与缓存 miss 兜底口径

### 安全

- 移除硬编码 Gitee Token，改环境变量
- 移除 Gitee 镜像入口（镜像仓库已删除）

---

## 安装

```bash
go build -o xuanji-server ./cmd/server
./xuanji-server --port 8787 --db ./xuanji.db
```

或下载下方三平台二进制（linux-amd64 / linux-arm64 / windows-amd64）。

首次启动访问 `http://<host>:8787/`，默认账号 `admin` / `xuanji123`（请尽快修改）。

## 测试

499 个测试全绿，覆盖协议转换、路由选择、熔断、思考归一化与最佳等级注入、时间线会话指纹。
