# 交接文档：CodeBuddy 免费模型不应被「积分耗尽」排除（含动态识别免费模型的可行性探测）

- 时间：2026-11-14 12:45 (+08:00)
- 仓库：/data/codes/xuanji（分支主库，HEAD d903206）
- 生产二进制：/data/xuanji/xuanji-server（11-13 23:08 构建）
- 生产 DB：/ssd/migrated/xuanji/xuanji.db（进程：`xuanji-server --port 3001 --db /ssd/migrated/xuanji/xuanji.db`）

## 1. 问题

codebuddy 渠道两个国际版账号（id 20 `icefairy`、id 21 `icefairy333@gmail.com`，domain `www.codebuddy.ai`）
`usage_left = 0 / usage_total = 190(160)`，导致：

- `internal/provider/codebuddy/pool.go:223` 的 `PickAccount` 把它们归入 fallback（`IsCreditExhausted()` 只看
  `left<=0 && total>0`，不看本次请求用的模型），正常轮询永远不选它们；
- 池里另有 8 个国内账号可用，fallback 也从不启用 → 两个国际版账号 **今天 0 次请求**
  （`vendor_use_daily` 中 account 20/21 = 0，`last_used_at` 停在 11-09 / 11-11）。
- 但用管理接口 `/test`（走 hy3 免费模型）单独探测，两个账号都 `{"ok":true,"probed":true}` ——
  **账号本身能跑免费模型**，纯粹是选号逻辑不给机会。

期望行为：请求模型属于「真正免额度」的模型时，`IsCreditExhausted()` 不应参与排除。

## 2. 已探测的事实（本轮实测，可直接复用）

### 2.1 代码位置

| 位置 | 作用 |
|---|---|
| `internal/provider/codebuddy/pool.go:208` `PickAccount(exclude map[int64]bool)` | 选号入口，第 223 行做耗尽判定 |
| `internal/provider/codebuddy/forward.go:89` | 唯一调用点，`opts.ForwardOptions` 里已有 `Body`（可 `gjson.GetBytes(opts.Body,"model")`） |
| `internal/provider/codebuddy/catalog.go` | 内置模型清单 `builtinCatalog`，`CatalogModel{Key,DisplayName,Credit,Free}`，当前标 Free 的是 `deepseek-v4.1-flash`、`hy3`、`hy4free` |
| `internal/provider/vendor/vendor.go:56` `ForwardOptions` | 共享契约；**不要改这里加 Model 字段**（qoder/cline 也在用），直接在 forward.go 里从 Body 取 |
| `internal/store/vendor_accounts.go:120` `IsCreditExhausted()` | `usage_left<=0 && usage_total>0` |

### 2.2 「标价 x0.00」≠「免额度」（重要）

catalog.go 注释里已有 2026-11-09 实测：账户周期 credits 耗尽时
`deepseek-v4.1-flash` 回 **429 code 14018**，而 `hy3` / `hy4free` 照常 200。
所以**不能**用 `Credit == "x0.00"` 判断免额度，必须区分：
- 标价 0 且不校验额度：`hy3`、`hy4free`（可安全放行耗尽账号）
- 标价 0 但仍校验额度：`deepseek-v4.1-flash`

### 2.3 能否「动态获取」免费模型清单 —— 探测结论：**没有官方接口，只能间接获取**

逐个试过（都用 id 20 的真实 access_token）：

| 探测 | 结果 |
|---|---|
| `GET /v1/models`、`GET /v2/models` | 404 `{"error_msg":"404 Route Not Found"}`（两个平台都一样） |
| `GET /v1/product`、`GET /api/v1/product`、`GET /v1/product/config` | 404 |
| `GET /v3/config`（无鉴权） | 200，`data.models = null`，只有 `agent/mcp/codebase/features` 空壳 |
| `GET /v3/config` + `Authorization` + `__codebuddyProductConfigurationRequest: true` + UA `codebuddy/2.178.1` | 200，返回 `enterpriseId/productFeatures/productFeaturesConfig`（只有 `ModelTrialBanner` 一个 hy4-preview 试用横幅），**仍然没有 models** |
| `POST /v2/billing/meter/get-user-resource` 直连 | 404（该接口要 workbuddy 专用头，见 checkin.go 的 workBuddyHeaders） |

即：**上游服务端不下发模型目录与计费属性**。模型清单与 `credits: "x0.00 credits"` 标价
只存在于官方 CLI 内置的 `product.json`（桌面端 `cli/product.json`；CLI 二进制里是内嵌 JSON）。

已验证：`strings` 提取本机 CLI 二进制即可拿到完整模型清单（含 credits 字段与 `id`）：
```bash
ls /root/.local/share/codebuddy/versions/   # 2.181.0 最新
strings -n 6 /root/.local/share/codebuddy/versions/2.181.0/codebuddy > /tmp/cbstr.txt
grep -n 'x0\.00 credits' /tmp/cbstr.txt     # 命中 deepseek-v4.1-flash / hy3 / echo / hy3-ioa
```
`product.json` 里每个模型形如：
```json
{ "credits": "x0.00 credits", "id": "hy3", "name": "Hy3", "vendor": "j",
  "maxOutputTokens": 64000, "maxInputTokens": 192000, "supportsToolCall": true, ... }
```

### 2.4 因此「动态」只能做到两层，不能 100% 自动

1. **动态拿模型名 + 标价**：从本机 CLI 二进制的 `product.json` 提取（`strings` + 找 `"credits"`/`"id"` 配对），
   得到 `{id: credits}` 表，可覆盖未来新增模型 —— 但仍是「标价 0」而非「免额度」。
2. **动态判定免额度**：只能**探测**。做法：仅对「标价 x0.00」的模型，用一个已知耗尽账号
   （`IsCreditExhausted()` 为真，如现在的 20/21）发一条最小请求（max_tokens=1），
   看返回：429 且 `code==14018` → 校验额度（不免）；200 → 免额度。
   结果缓存进 `config` 表（key 如 `codebuddy.quota_exempt_models`，值为 JSON 数组 + 探测时间 + 探测所用账号），
   每 N 小时/签到后复检一次，失败保留上次结果。
3. 兜底：内置白名单 `hy3` / `hy4free` 永远视为免额度（避免探测抖动导致流量全断）。

> 结论：**没有纯查询式的动态获取途径**。可行的是「CLI 清单提取（模型列表+标价）」+「429/14018 探测（免额度判定）」两段式，外加内置兜底。

## 3. 实施方案（**已实施、已部署、已线上验证**）

### 3.1 最小改动（已完成）

`internal/provider/codebuddy/pool.go`
- 新增 `PickAccountForModel(exclude map[int64]bool, model string)`；
- `PickAccount(exclude)` 改为 `PickAccountForModel(exclude, "")` 的薄包装（保持现有测试与调用兼容）；
- 在遍历循环里：
```go
if p.IsCoolingDown(a.ID) { fallback = append(fallback, a); continue }
if a.IsCreditExhausted() && !IsQuotaExemptModel(model) {
    fallback = append(fallback, a); continue
}
```

`internal/provider/codebuddy/catalog.go`
- 新增 `func IsQuotaExemptModel(model string) bool`：命中 `quotaExemptBuiltin`（`hy3`、`hy4free`）；
  归一规则：去空格、小写、去掉上游回显后缀（如 `hy4-free` → `hy4free`、`hy3-ioa` 不算）；
- 预留 `SetQuotaExemptModels([]string)`（由探测结果/config 注入，进程内在 `NewPool`/热重载时设置）。

`internal/provider/codebuddy/forward.go`
- 第 89 行改为：
```go
model := gjson.GetBytes(opts.Body, "model").String()
acct, err = p.PickAccountForModel(tried, model)
```

测试 `internal/provider/codebuddy/pool_test.go` 或 `forward_test.go`：
- 耗尽账号 + 免费模型 → 能被选中；
- 耗尽账号 + 付费模型 → 仍进 fallback；
- 全部账号耗尽 + 付费模型 → 退化轮询（原行为不变）。

### 3.2 第二阶段（可选，动态化）

- 新文件 `internal/provider/codebuddy/freemodel.go`：
  - `RefreshQuotaExemptModels(ctx)`：①读 CLI `product.json`（二进制 strings 提取或安装目录）拿 `credits=x0.00` 的 id 集合；
    ②对其中每个 id 用耗尽账号探测（`max_tokens=1`，超时 20s）；③写 `config` 表缓存；
  - 调度：跟随现有 `StartCreditRefresh`（每 10 分钟一次太频繁，建议 6 小时）或签到后触发一次。
- 需要 CLI 二进制路径配置：优先 `upstream.<name>.cli_path`，否则扫 `~/.local/share/codebuddy/versions/*/codebuddy` 取最高版本。
- 风险：探测会给上游打真实请求（每模型一次，一天几十次），且探测失败不能影响现有白名单。

### 3.3 部署

官方脚本：`/data/codes/xuanji/deploy.sh`（go build 注入 buildDate → supervisorctl stop/start → healthz 校验 → 失败自动回滚）。
```bash
cd /data/codes/xuanji && ./deploy.sh          # 默认端口 3001
```
构建前必须导出环境（本机 HOME 为空）：
```bash
export HOME=/root GOCACHE=/root/.cache/go-build GOPATH=/root/go GOMODCACHE=/root/go/pkg/mod
```
测试：`go test ./internal/provider/... ./internal/admin/...`（已全绿）。


## 4. 部署中断核查结论（已完成）

- [x] `/proc/1646701/exe` → `/srv/dev-disk-by-uuid-.../xuanji/xuanji-server`，
      与 `/data/xuanji/xuanji-server` 是同一 inode（bind/软链，md5 都是
      `6f06c425bfe04d459c26c404be388b06`）→ 跑的就是这份二进制
- [x] 二进制 mtime 2026-11-13 23:08:00，进程启动时间 Nov 13 23:08:00，**一致**
- [x] `curl localhost:3001/healthz` → 200
- [x] supervisor: `xuanji RUNNING pid 1646701`
- 结论：**11-13 那次部署是成功的**，会话中断只影响了后续动作，二进制与进程都是新的。
  但要确认「新代码是否含本次免费模型改动」——不含（本次改动尚未部署）。

## 5. 顺带发现（不在本次范围）

- 今日 codebuddy 签到 20/21 未自动跑（用户已手动点，`last_checkin_date` 仍是 11-13）。
  11-14 日志里 codebuddy 签到记录为 0 条，建议单独排查 `Scheduler` 在 11-13 23:08 重启后的
  定时器重建逻辑（05:00–09:00 窗口是否被跳过）。

## 6. 部署与线上验证（2026-11-14 12:44，已完成）

- 构建+部署：`cd /data/codes/xuanji && ./deploy.sh`（备份 `/data/xuanji/xuanji-server.bak-20261114-124421`）
- `healthz` → `{"build_date":"2026-11-14","status":"ok"}`，supervisor `xuanji RUNNING pid 2357419`
- 提交：`a15ff02 fix(codebuddy): 免费模型(hy3/hy4free)不再因积分耗尽排除账号`
- 线上验证：用网关 key 连打 4 次 `model=hy4free`（`POST /v1/chat/completions`），全部 200；
  紧接着查库：
  ```
  20 icefairy              last_used_at 1794631494548  use_count 5688
  21 icefairy333@gmail.com last_used_at 1794631496573  use_count 3704
  ```
  两个国际版账号的 `last_used_at` / `use_count` **都动了**（改动前分别停在 11-09 / 11-11），
  说明免费模型流量已重新分担到它们身上。国内账号也照常承接（轮询混合）。
