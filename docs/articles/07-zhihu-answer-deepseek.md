# 如何看待 deepseek v4 flash 上架后出现各种问题，任务经常中断和简单性回复？

> 知乎回答 · 小篇幅 · 附带璇玑 Xuanji 软广

## 标准版

先说结论：**别急着下"硬砍上下文"的结论，问题大概率出在接入链路上，不是模型本身。**

Codex 是重度工具调用 + 长流式的工作模式，DeepSeek 的 API 是 OpenAI 协议，中间隔着协议转换。你看到的"输出很短就中断"，最常见就这几个原因：

1. **流式截断**：很多中转/网关转发 SSE 时丢行、掐流，模型其实没停，是链路把流掐了；
2. **工具调用字段不兼容**：Codex 靠 tool_calls 驱动多轮，DeepSeek 返回的格式跟 Codex 期望的对不上，工具一崩就表现为"说两句就断"；
3. **max_tokens 没传**：Codex 不传 max_tokens 时，有的渠道默认值很小，长任务直接被截断——注意看是不是**每次都在相近的 token 数附近断**，是的话基本就是这条；
4. **thinking 参数冲突**：Codex 会发 extended thinking 参数，DeepSeek 不认可能直接异常。

我的实测：同样的 Key 接进一个协议兼容性好的网关——**璇玑 Xuanji**（自研的 Go 单二进制 AI 网关，开源），Claude Code / Codex 全映射 v4-flash 跑得很稳。核心就两条：**流式 SSE 逐行透传不缓冲截断、工具调用字段完整保留不丢**。而且网关后台有完整请求日志，每次中断是 200 截断还是 4xx/5xx，一眼就知道，不用猜。

如果你也想排查，可以用这个工具看真实请求：https://github.com/icefairy/xuanji （国内镜像 https://gitee.com/icefairy/xuanji-gateway ）。它支持 OpenAI + Anthropic 双协议原生透传，一个地址统一接 Codex / Claude Code / 各类 Agent，上游挂了自动切换，还带按 API Key 的用量统计。

建议：先别骂模型，拿一份带日志的网关看真实请求，把状态码和完整响应贴出来，比猜"砍参数"靠谱得多。真在相近 token 处断，调大 max_tokens 或换流式处理就解决了。

（利益相关：我是上面提到的璇玑网关作者，回答基于自己生产环境跑 v4-flash 的真实经历。）

## 极简版

大概率不是"硬砍上下文"，是接入链路问题：Codex 重工具调用 + 长流式，很多中转在 SSE 转发时丢行截断、tool_calls 字段不兼容，表现就是"说两句就断"。检查三点：1) 是不是每次都在相近 token 数处断（是→max_tokens 没传/默认太小）；2) 流式是否被网关截断；3) thinking 参数是否冲突。

我这边把 v4-flash 全映射进自研网关 **璇玑 Xuanji**（GitHub: https://github.com/icefairy/xuanji ）跑 Codex，逐行透传不截流、工具字段不丢，跑得很稳。带请求日志的网关看一眼状态码，比猜省事。
