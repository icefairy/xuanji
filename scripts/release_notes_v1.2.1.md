# v1.2.1

## 修复：时间线注入的「距上轮」在 agent 客户端下永不显示

v1.2.0 上线后，注入消息里只有【时间】，始终没有【距上轮】。感谢实际使用暴露的问题。

### 根因

会话指纹的采样窗口太窄：

| | v1.2.0 | v1.2.1 |
|---|---|---|
| 参与哈希的消息 | 首 4 + **末 3** | 首 4 + **末 6** |
| 回退匹配深度 | **3** 条 | **12** 条 |

Hermes、Claude Code 这类 agent 客户端，**一轮对话内**会插入多条 `tool_call` / `tool_result`，历史增量常常超过 3 条。此时上一轮的「末 3 条」与这一轮的「末 3 条」毫无交集，回退 3 条也命不中上一轮前缀，于是**每一轮都被判成新会话**，`haveGap` 恒为 false，只剩时间行。

### 修复

- 采样窗口 `首 4 + 末 3` → `首 4 + 末 6`（`maxFingerprintMessages` 7 → 10）
- `callbackKeys` 回退深度 3 → 12（新增 `maxCallbackCuts`）

开销不变：仍只哈希 10 条消息、单条内容截断 2KB；最多多算 12 次短序列哈希，可忽略。

### 升级提示

升级后**头一两轮仍可能只有时间**：会话表里按旧窗口算出的指纹与新代码对不上，需要重新建立。此后「距上轮」稳定显示。

### 测试

新增两个回归用例：

- `AgentToolBurst` —— 一轮内插 5 条工具消息，仍识别为同一会话并显示间隔
- `ShouldHashMessageStable` —— 采样只依赖 `(i, n)`，保证前缀稳定性

---

## 完整安装

```bash
go build -o xuanji-server ./cmd/server
./xuanji-server --port 8787 --db ./xuanji.db
```

或下载下方三平台二进制（linux-amd64 / linux-arm64 / windows-amd64）。

时间线注入默认关闭，在「系统设置 → 时间线注入」一键开启。详见 [README](https://github.com/icefairy/xuanji#️-时间线注入让模型知道过了多久)。
