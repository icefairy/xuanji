# 别再做"模型参数翻译官"了：我把 6 种模型的思考开关，归一成了 1 个参数

> Go · OpenAI/Anthropic 双协议 · 思考深度归一化（Thinking Effort 协议翻译）
> 开源地址在文末。

## 被思考参数逼疯的下午

上个月我在网关里接了一批"思考模型"：DeepSeek V4、商汤日日新、Kimi K3、GLM-4.5、Qwen3.6……本以为 OpenAI 协议嘛，客户端写好一次就完事。结果每个模型告诉我：

- DeepSeek 说：思考强度用 `reasoning_effort: low/high/max`，关思考用 `thinking.type: disabled`；
- 商汤说：不对，我用 `output_config: {effort: low/medium/high}`；
- Kimi K2 说：我只有 `thinking.type` 开关，没有强度档；
- Qwen3.6 说：我不用那些，我用 `enable_thinking` + `thinking_budget`；
- Qwen2.5 说：我压根没有思考模式，别给我传！

同一个需求——"这道题别想太深，快点答"——在 6 个模型里要写 6 种不同的 JSON。客户端每接一个新模型，就要改一次代码。**我成了模型的参数翻译官。**

## 思路：在网关层做"协议翻译"

想通之后特别简单——**客户端永远说"普通话"（OpenAI 标准协议），网关负责翻译成每个模型能听懂的"方言"**。

客户端只需传一个参数：

```json
{
  "model": "任意模型",
  "reasoning_effort": "low"
}
```

网关按上游真实模型名自动转换。归一化矩阵：

| 目标模型族 | none（关闭思考） | 强度控制 |
|---|---|---|
| **DeepSeek V4** (flash/pro) | `thinking.type=disabled` | `reasoning_effort: low/high/max`（原生透传，pro 的 low 档自动抬到 high） |
| **商汤** sensenova-* | `thinking.type=disabled` | 自动转 `output_config.effort: low/medium/high` |
| **Kimi K3** | 始终思考 → 映射为 `low` | `reasoning_effort: low/high/max`（原生透传） |
| **Kimi K2.x** | `thinking.type=disabled` | 只开关，无强度档 → `thinking.type=enabled` |
| **GLM-4.5** | `thinking.type=disabled` | 只开关，无强度档 → `thinking.type=enabled` |
| **Qwen3 / 3.5 / 3.6 / 3.7** | `enable_thinking=false` | 无 effort 档 → `enable_thinking=true` + `thinking_budget` 分档（low=1024 / medium=4096 / high=8192） |
| **OpenAI o3/o4/GPT-5** | `reasoning_effort=none` | 原生支持，完全透传 |

## 三个容易踩的坑（我都踩过了）

**坑一：Qwen 系列不是同一个物种。** Qwen3、3.5、3.6、3.7 都是混合思考模型（`enable_thinking` + `thinking_budget`），但 Qwen3.5 开源小模型**默认禁用思考**，不显式开启就没推理；Qwen3.6 默认思考可关闭，且**不支持** Qwen3 的 `/think` `/no_think` 提示词软切换。而 **Qwen2.5 根本没有思考模式**——归一化匹配必须排除，否则会把思考参数塞给一个不认识的模型。

**坑二：映射要按"上游真实模型名"判断，不能按客户端名。** 客户端可能叫 `qwen3.6:35b`，但网关把它映射到商汤 `sensenova-6.7-flash-lite` 了——这时候必须按映射后的真实名走商汤的转换，而不是 Qwen 的。璇玑在 model_mapping 之后做归一化，天然不会张冠李戴。

**坑三：Kimi K3 永远关不掉思考。** 它是纯推理模型，`none` 档只能映射到 `low`（最接近"少想"的档位），不能真关闭。归一化要理解模型的"不可能"，而不是硬塞一个不存在的参数。

## 实测：思考强度真的生效

归一化不只是"参数映射过去不报错"，而是强度真的要变。我用 57 题评测集（知识 20 + 逻辑 37，含判断/选择/数值/可运行代码）跑了两套模型的全档位：

**商汤 Lite（走归一化，`output_config.effort`）**

| 档位 | 得分 |
|---|---|
| none（关思考） | 51/57 |
| **low** | **53/57** |
| medium | 52/57 |
| high | 52/57 |

**DeepSeek Flash（走归一化，`reasoning_effort` 透传）**

| 档位 | 得分 |
|---|---|
| none（关思考） | 51/57 |
| low | 51/57 |
| **high** | **54/57** |
| max | 53/57 |

两个模型关思考时得分完全一样（51/57）——说明知识储备相同，**差异全在推理**。商汤最优点在 low（93%），DeepSeek 最优点在 high（94.7%）。思考强度参数真实控制推理深度，归一化没把参数转丢。

评测脚本和报告都开源在仓库里：`tools/eval_models.py` + `docs/reports/model-eval-20260803.md`。

## 一个参数接入所有思考模型

现在我的每个 AI 程序只需要知道一件事：**想要快就 `low`，想要准就 `high`，不想让它想就 `none`**。模型换了、上游换了、参数协议换了，客户端一行代码都不用改。

- 请求体没有 `reasoning_effort` 时**零开销**（原样透传，不改 body）；
- 未知模型也原样透传，绝不破坏请求；
- 单条请求即可控制，无需每模型单独适配——**一套代码接入所有思考模型**。

## 开源地址

- GitHub：https://github.com/icefairy/xuanji
- Gitee 镜像（国内快）：https://gitee.com/icefairy/xuanji-gateway

Star 支持一下，有问题欢迎提 Issue。

---

*个人业余作品，没有计费功能，短期内也不打算加——个人自用，够了。*
