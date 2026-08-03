# 同一个"思考深度"，DeepSeek 用 reasoning_effort、商汤用 output_config、Qwen 用 enable_thinking——我把它们全归一成了 1 个参数

> Go · OpenAI/Anthropic 双协议 · 思考深度归一化（Thinking Effort 协议翻译）
> 开源地址在文末。

## 6 个模型，6 套思考参数

现在是个模型就带"思考模式"，但每家的开关长得都不一样：

| 模型 | 关思考 | 控制强度 |
|---|---|---|
| DeepSeek V4 | `thinking.type=disabled` | `reasoning_effort: low/high/max` |
| 商汤 | `thinking.type=disabled` | `output_config.effort: low/medium/high` |
| Kimi K3 | 关不掉（纯推理） | `reasoning_effort: low/high/max` |
| Kimi K2.x | `thinking.type=disabled` | 没有强度档 |
| GLM-4.5 | `thinking.type=disabled` | 没有强度档 |
| Qwen3/3.5/3.6/3.7 | `enable_thinking=false` | `enable_thinking` + `thinking_budget` |
| OpenAI o3/o4 | `reasoning_effort=none` | `reasoning_effort: minimal~max` |

客户端接一个模型，就要适配一套参数。接 6 个模型，就是 6 套 if-else。而且这只是**现在**的模型——下个月出了新模型，参数又不一样，还得改。

## 归一化：客户端说普通话，网关做翻译

思路一句话：**客户端永远用 OpenAI 标准协议传 `reasoning_effort`，网关按上游真实模型自动翻译成对方要的参数。**

```json
{
  "model": "任意模型",
  "reasoning_effort": "high"   // 就是 OpenAI o3 用的同一个字段
}
```

- 传 `none` → 网关帮你在目标模型上关思考；
- 传 `low / medium / high` → 网关映射到该模型的实际强度档位；
- DeepSeek 的 `reasoning_effort` 原生就支持 → 透传；
- 商汤只认 `output_config.effort` → 自动转；
- Qwen3 没有 effort 概念 → 转成 `enable_thinking=true` + `thinking_budget` 分档（low=1024 / medium=4096 / high=8192）。

**关键细节：按"上游真实模型名"判断，不是客户端名。** 客户端叫 `qwen3.6:35b`，可能被路由映射到了商汤的 `sensenova-6.7-flash-lite`——此时按真实名走商汤的转换，绝不张冠李戴。

## 实测：归一化没有把参数转丢

我跑了一个 57 题的评测集（知识 20 + 逻辑/数学/代码 37），验证归一化后思考强度真的生效：

**商汤 Lite 走归一化（转 `output_config.effort`）：**

| 档位 | 得分 |
|---|---|
| none | 51/57 |
| **low** | **53/57** |
| medium | 52/57 |
| high | 52/57 |

**DeepSeek Flash 走归一化（`reasoning_effort` 透传）：**

| 档位 | 得分 |
|---|---|
| none | 51/57 |
| low | 51/57 |
| **high** | **54/57** |
| max | 53/57 |

两模型关思考都 51 分——知识一样，差异全在推理。归一化后各档位分数梯度清晰，说明参数真实传到了上游并生效。

## 为什么这是网关该干的事

因为**客户端和模型是一对多的关系**。你有 3 个 AI 程序（Claude Code、脚本、网页），每个都要接 7 个模型——如果是客户端适配，那是 3×7 = 21 处适配；如果在网关归一化一次，那是 1 处翻译，21 处全受益。

这就是网关存在的意义：**把"模型间的差异"收敛到网关这一层，不让差异扩散到每个客户端。**

- 请求体没有 `reasoning_effort` 时零开销原样透传；
- 未知模型原样透传，绝不破坏请求；
- 单条请求即可控制思考深度，无需每模型单独适配。

## 开源地址

- GitHub：https://github.com/icefairy/xuanji
- Gitee 镜像（国内快）：https://gitee.com/icefairy/xuanji-gateway

Star 支持一下，有问题欢迎提 Issue。

---

*个人业余作品，没有计费功能，短期内也不打算加——个人自用，够了。*
