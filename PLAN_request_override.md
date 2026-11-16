# 请求体复写功能（Request Override）实现计划

## 目标
给璇玑网关每个上游增加一个「请求体复写」配置：不管客户端传什么，网关转发前强制覆盖请求体 JSON 中的部分字段。
典型用途：关思考加快响应、固定采样参数。
例如配置 `{"chat_template_kwargs":{"enable_thinking":false},"temperature":0.3}`：
- 客户端传的任何 temperature 被强制覆盖为 0.3
- 请求体若无 chat_template_kwargs 则补上；有则只覆盖 enable_thinking=false，保留其他子键

## 设计
- 字段名：`request_override`（DB 列 / UpstreamRow / Upstream / admin API / 前端）
- 存 JSON 字符串（嵌套对象），如 `{"chat_template_kwargs":{"enable_thinking":false},"temperature":0.3}`
- 空 / "{}" / 空串 = 不启用
- 语义：**深度合并覆盖**——配置里出现的键，覆盖请求体同名键；请求体没有的键则新增；嵌套对象逐层合并，未出现的子键保留请求体原值

## 改动点（全部文件）

### 1. store.go（internal/store/store.go）
- `UpstreamRow` 加字段 `RequestOverride string \`json:"request_override"\``
- `ensureColumn(s.db, "upstreams", "request_override", "request_override TEXT NOT NULL DEFAULT ''")`（在 380 行 billing_exempt 附近加）
- `CreateUpstream`：INSERT 语句加 `, request_override` 列 + 值
- `UpdateUpstream`：UPDATE 的 `setExpr` 基础列加 `request_override=?`（作为非可空基础列，跟随 name/type/base_url 一起更新即可，不需要 ptr）
- `ListUpstreams` / `GetUpstream`（及任何 SELECT upstreams 的查询）：SELECT 列加 `request_override`
- `CloneUpstream`：复制时带 `RequestOverride`

### 2. config.go（internal/config/config.go）
- `Upstream` struct 加 `RequestOverride string \`yaml:"request_override"\``
- `LoadFromStore` 构造 Upstream 处：`RequestOverride: up.RequestOverride`（与 ModelMapping 赋值同位置）

### 3. admin.go（internal/admin/admin.go）
- `AddUpstream` / `UpdateUpstream` handler：request_override 是普通字符串字段，随 JSON body 自动 decode 进 UpstreamRow，无需额外处理（与 models/base_url 同级）
- `ListUpstreams` handler 响应自动带上（UpstreamRow 已含该字段）
- 检查 `UpdateUpstream` 是否对缺失 request_override 有特殊处理——按**普通字符串直接覆盖**即可（与 base_url 一致，不是 0/1 开关不需要 ptr）

### 4. proxy.go（internal/proxy/proxy.go）— 核心注入
在 forwardOnce 里 `normalizeThinkingEffort` 之后、流式 `stream_options.include_usage` 注入之前，调用：
```go
if nb, changed := applyRequestOverride(reqBody, up.RequestOverride); changed {
    reqBody = nb
}
```
新增函数 `applyRequestOverride`：
- override 为空/空串/"{}" → 返回原样 false
- 解析 override 为 `map[string]interface{}`
- 递归把嵌套 map 展开成点路径（如 `chat_template_kwargs.enable_thinking`），对每个**叶子**值 `sjson.SetBytes(body, path, value)`（bool/数字/字符串/nil 都支持）
- 有任一 Set 成功 → 返回 true
- 解析失败（非法 JSON）→ 返回 false 原样转发（不阻断），可打日志

### 5. 前端 admin_vue.html（cmd/server/web/admin_vue.html）
- 编辑上游弹窗：model_mapping 文本域（2143 行附近）后加「请求体复写」文本域（textarea，v-model="editForm.request_override"，placeholder 示例 `{"chat_template_kwargs":{"enable_thinking":false},"temperature":0.3}`，提示「覆盖转发给上游的请求参数，空=不启用」）
- data `editForm` 初始化加 `request_override: ""`
- `doEditUpstream` PUT body 加 `request_override: f.request_override`
- 编辑弹窗回填：`openEdit` 里 `request_override: u.request_override || ""`
- **同 model_mapping 的规范化保护**：request_override 可能是对象或字符串，统一 `JSON.stringify` 成字符串再编辑（参照 4294-4300 行的 model_mapping 处理）

### 6. 测试
- proxy 层单测：`applyRequestOverride` 各场景
  - 空 override → 不变
  - 顶层标量覆盖（temperature）
  - 嵌套对象新增（chat_template_kwargs 不存在时补全）
  - 嵌套对象合并（chat_template_kwargs 已有其他键，只覆盖 enable_thinking 保留其他键）
  - 非法 JSON → 原样
- 文件：internal/proxy/request_override_test.go

## 验收标准
1. `go build ./...` 通过
2. `go test ./...` 全绿（含新增单测）
3. 管理页：上游编辑弹窗出现「请求体复写」输入框，能保存/回显
4. 真实请求验证：给某上游配 `{"temperature":0.3}`，客户端传 temperature=0.7，转发到上游的是 0.3（用 TestUpstream 或抓上游请求确认）；配 `{"chat_template_kwargs":{"enable_thinking":false}}` 给 flash/deepseek 关闭思考，响应速度提升且无 reasoning_content
