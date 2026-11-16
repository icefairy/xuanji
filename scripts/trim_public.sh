#!/bin/bash
# 从 main 裁剪出 public 版本：移除厂商账号池（CodeBuddy）私有功能。
#
# 设计（2026-09-17 引入）：public 分支不再靠 cherry-pick 纯核心修复维护——
# 那种方式每次都会撞上 cmd/server/main.go（因提交 parent 链带 vendor 接线），
# 实测 merge-tree 确认冲突。改为：public 始终是 main 的「裁剪副本」，
# 同步时 git merge main 后重跑本脚本（幂等）。
#
# 用法:
#   ./scripts/trim_public.sh            # 在当前分支上执行裁剪
#   ./scripts/trim_public.sh --dry-run  # 只看会做什么
#
# 保留（通用功能，非 codebuddy 专属）：
#   internal/config/fingerprint.go + piheaders/  —— 配置上游 UA/请求头指纹，被 config.go 正常引用
#   config.go 的 Vendor/VendorEndpoint 字段与 IsCodeBuddy()/UsesVendorPool()
#     —— 纯字符串逻辑，不 import provider 包；保留可避免动结构体（最小改动）
set -euo pipefail

DRY=0
[ "${1:-}" = "--dry-run" ] && DRY=1
cd "$(dirname "$0")/.."

say() { echo "==> $*"; }
run() { if [ "$DRY" = 1 ]; then echo "    [dry] $*"; else eval "$@"; fi; }

# ── 1. 删除厂商账号池专属文件 ─────────────────────────────────────────
VENDOR_FILES=(
  internal/provider/codebuddy
  internal/admin/vendor_accounts.go
  internal/admin/vendor_import.go
  internal/admin/vendor_import_test.go
  internal/proxy/vendor_forward.go
  internal/proxy/vendor_forward_test.go
  internal/proxy/vendor_stream_record_test.go
  internal/store/vendor_accounts.go
  internal/store/vendor_accounts_test.go
)
say "1/4 删除厂商账号池专属文件"
for f in "${VENDOR_FILES[@]}"; do
  [ -e "$f" ] || { echo "    (已不存在) $f"; continue; }
  run "rm -rf '$f'"
  echo "    移除 $f"
done

# ── 2. 剥离 cmd/server/main.go 的 vendor 接线 ─────────────────────────
say "2/4 剥离 cmd/server/main.go"
if [ "$DRY" = 1 ]; then
  echo "    将删除：codebuddy import、签到调度注册、vendor 路由、registerVendorPools/vendorPool/setVendorPool/StartAllVendorCheckinSchedulers 等函数"
else
  python3 - <<'PYEOF'
import re, pathlib
p = pathlib.Path("cmd/server/main.go")
src = p.read_text(encoding="utf-8")

# a) 删 codebuddy import
src = re.sub(r'\n\t"github\.com/icefairy/xuanji/internal/provider/codebuddy"\n', '\n', src)

# b) 删签到调度注册块（注释 + stopCheckin 调用 + 其 defer）
src = re.sub(
    r'\n\t// 厂商账号（CodeBuddy）每日自动签到[^\n]*\n(?:\t//[^\n]*\n)*\tstopCheckin := StartAllVendorCheckinSchedulers\([^\n]*\)\n(?:\tdefer stopCheckin\(\)\n)?',
    '\n', src)

# c) 删 vendor 路由块（/admin/* 与 /api/admin/* 两组，中间可能隔着其它路由注释）
src = re.sub(
    r'\n\t// 厂商账号（CodeBuddy）：账号列表/导入[^\n]*\n(?:\t//[^\n]*\n)*'
    r'(?:\tadmHandler\.SetVendorPool\([^\n]*\)\n)?'
    r'(?:\tmux\.HandleFunc\("(?:GET|POST) /(?:api/)?admin/vendor[^\n]*\)\n)+',
    '\n', src)

# c2) 兜底：删 /api/admin/vendor* 路由（可能前缀带注释行）
src = re.sub(
    r'\n(?:\t//[^\n]*\n)*\t// 同时开放到 /api/admin/\*[^\n]*\n(?:\tmux\.HandleFunc\("(?:GET|POST) /api/admin/vendor[^\n]*\)\n)+',
    '\n', src)
src = re.sub(r'\n\tmux\.HandleFunc\("(?:GET|POST) /api/admin/vendor[^\n]*\)\n', '\n', src)
src = re.sub(r'\n\tmux\.HandleFunc\("(?:GET|POST) /(?:api/)?admin/vendor[^\n]*\)\n', '\n', src)

# d) 删 registerVendorPools 调用（含前置注释）
src = re.sub(
    r'\n\t// 厂商账号池（codebuddy）：账号矩阵型上游的选号/刷新/换号重试。\n(?:\t//[^\n]*\n)*\tregisterVendorPools\([^\n]*\)\n',
    '\n', src)

# e) 删函数定义：registerVendorPools / vendorPool / setVendorPool / StartAllVendorCheckinSchedulers / sameHost(若仅此处用)
for fn in ("registerVendorPools", "vendorPool", "setVendorPool", "StartAllVendorCheckinSchedulers"):
    src = re.sub(
        r'\n(?:// [^\n]*\n)*func ' + fn + r'\([^\n]*\{\n(?:.*?\n)*?\}\n',
        '\n', src, count=1)

# f) 删 vendor 全局变量块
src = re.sub(
    r'\n(?:// [^\n]*\n)*var \(\n\tvendorPoolsMu sync\.Mutex\n\tvendorPools   = map\[string\]\*codebuddy\.Pool\{\}\n\)\n',
    '\n', src)

p.write_text(src, encoding="utf-8")
print("    main.go 已裁剪")
PYEOF
fi

# ── 3. 剥离 internal/proxy/proxy.go ──────────────────────────────────
say "3/4 剥离 internal/proxy/proxy.go"
if [ "$DRY" = 1 ]; then
  echo "    将删除：codebuddy import、vendorPools 字段、SetVendorPool/vendorPool 方法、forwardViaVendorPool 分支"
else
  python3 - <<'PYEOF'
import re, pathlib
p = pathlib.Path("internal/proxy/proxy.go")
src = p.read_text(encoding="utf-8")

src = re.sub(r'\n\t"github\.com/icefairy/xuanji/internal/provider/codebuddy"\n', '\n', src)

# 字段声明
src = re.sub(
    r'\n\t// vendorPools 厂商账号池[^\n]*\n(?:\t//[^\n]*\n)*'
    r'\tvendorPools   map\[string\]\*codebuddy\.Pool\n'
    r'\tvendorPoolsMu sync\.RWMutex\n',
    '\n', src)

# SetVendorPool / vendorPool 方法
for fn in ("SetVendorPool", "vendorPool"):
    src = re.sub(
        r'\n(?:// [^\n]*\n)*func \(h \*Handler\) ' + fn + r'\([^\n]*\{\n(?:.*?\n)*?\}\n',
        '\n', src, count=1)

# forwardViaVendorPool 分支：整块 if 替换为 todo 占位（保守：仅注释掉分支）
src = re.sub(
    r'\n\t// 厂商账号池上游（配了 vendor 的上游[^\n]*\n(?:\t//[^\n]*\n)*'
    r'\tif up\.UsesVendorPool\(\) \{\n(?:.*?\n)*?\t\}\n',
    '\n', src, count=1)

p.write_text(src, encoding="utf-8")
print("    proxy.go 已裁剪")
PYEOF
fi

# ── 6. 移除私用运维脚本（2026-09-17 发现：这些早已误传到公开的 public 分支）──
# 含具体服务器 IP/内网路径/日志路径/自动部署流程，属私用工具，不应公开。
# 保留：build.sh（通用编译）、create_*_release.sh（发版工具）、release_notes_*.md
#
# 注意：不能删本脚本自身——bash 边读边执行，删除正在运行的脚本会出错。
# 本脚本应在 main 上维护（public 上的副本由 git rm 移除）。
PRIVATE_SCRIPTS=(
  scripts/nightly_push.sh
  scripts/nightly_push.sh.bak.20260907-1205
  scripts/pi-autofix.sh
  scripts/deploy_remote.sh
)
# 含生产环境细节的开发文档，同样不应公开：
#   AGENTS.md —— 生产实例路径/supervisor 配置/内网 Gitea/数据库位置
#   PLAN.md / PLAN_*.md —— 早期规划（已过期）+ 示例里带内网地址
PRIVATE_DOCS=(
  AGENTS.md
  PLAN.md
  PLAN_request_override.md
  CLAUDE.md
  # deploy.sh —— 写死生产机路径 /data/xuanji/xuanji-server + supervisor 流程，属私用部署脚本
  # （公开用户用 Dockerfile / docker-compose.yml 部署）
  deploy.sh
)
say "6/6 移除私用运维脚本/文档 + 清洗示例配置"
for f in "${PRIVATE_SCRIPTS[@]}" "${PRIVATE_DOCS[@]}"; do
  [ -e "$f" ] || { echo "    (已不存在) $f"; continue; }
  run "git rm -q -f '$f'"
  echo "    移除 $f"
done
# 自身：在 public 上移除（用 git rm 目录索引，不影响已加载的脚本执行）
if [ -e scripts/trim_public.sh ]; then
  run "git rm -q -f --cached scripts/trim_public.sh"
  echo "    从 public 索引移除 scripts/trim_public.sh（保留工作区副本供后续 merge 后重跑）"
fi
# ── 4.8 清洗 config.example.yaml 里的内网地址 ────────────────────────
if [ -f config.example.yaml ]; then
  if [ "$DRY" = 1 ]; then
    echo "    将把 192.168.x.x / 10.x.x.x 示例地址改为占位域名"
  else
    python3 - <<'PYEOF'
import re, pathlib
p = pathlib.Path("config.example.yaml")
if p.exists():
    src = p.read_text(encoding="utf-8")
    new = re.sub(r'http://192\.168\.\d+\.\d+:\d+', 'https://your-upstream.example.com', src)
    new = re.sub(r'http://10\.\d+\.\d+\.\d+:\d+', 'https://your-upstream.example.com', new)
    if new != src:
        p.write_text(new, encoding="utf-8")
        print("    config.example.yaml 内网地址已清洗")
    else:
        print("    无需清洗")
PYEOF
  fi
fi

# ── 4. 剥离 admin.go / store.go ──────────────────────────────────────
say "4/6 剥离 internal/admin/admin.go 与 internal/store/store.go"
if [ "$DRY" = 1 ]; then
  echo "    admin.go: 删 codebuddy import + vendorPool 字段/方法"
  echo "    store.go: 删 initVendorAccountSchema 调用"
else
  python3 - <<'PYEOF'
import re, pathlib

p = pathlib.Path("internal/admin/admin.go")
src = p.read_text(encoding="utf-8")
src = re.sub(r'\n\t"github\.com/icefairy/xuanji/internal/provider/codebuddy"\n', '\n', src)
src = re.sub(
    r'\n\t// vendorPool 按厂商标识返回账号池[^\n]*\n(?:\t//[^\n]*\n)*\tvendorPool func\(vendor string\) \*codebuddy\.Pool\n',
    '\n', src)
src = re.sub(
    r'\n// SetVendorPool 注入厂商账号池解析器[^\n]*\nfunc \(h \*Handler\) SetVendorPool\([^\n]*\{[^\n]*\}\n',
    '\n', src, count=1)
# 补上 writeJSONStatus：它是 admin 通用工具（config.go 等也用），
# main 分支误放在 vendor_accounts.go 里 —— 裁剪时会把定义删掉、
# 但占位处仍被调用而编译断裂。这里在通用文件 admin.go 里重建。
if 'func writeJSONStatus' not in src:
    anchor = '// SetAuth 注入下游 key 鉴权器'
    src = src.replace(
        anchor,
        '// writeJSONStatus 以指定状态码写 JSON 响应（admin 通用工具）。\n'
        '// 注意：不放 vendor_accounts.go —— 通用处理器也用它，放厂商文件会导致\n'
        '// 厂商功能裁剪后编译断裂。\n'
        'func writeJSONStatus(w http.ResponseWriter, status int, v any) {\n'
        '\tw.Header().Set("Content-Type", "application/json")\n'
        '\tw.WriteHeader(status)\n'
        '\t_ = json.NewEncoder(w).Encode(v)\n'
        '}\n\n' + anchor, 1)

p.write_text(src, encoding="utf-8")
print("    admin.go 已裁剪")

p = pathlib.Path("internal/store/store.go")
src = p.read_text(encoding="utf-8")
src = re.sub(
    r'\n\t// 厂商账号（CodeBuddy 等需要 OAuth 登录态[^\n]*\n(?:\t//[^\n]*\n)*\tif err := initVendorAccountSchema\(s\.db\); err != nil \{\n(?:.*?\n)*?\t\}\n',
    '\n', src, count=1)
p.write_text(src, encoding="utf-8")
print("    store.go 已裁剪")
PYEOF
fi

say "完成。下一步必须验证："
echo "    go build ./... && go test ./..."
echo "    若报未使用 import/变量残留，按提示手工清理（脚本覆盖不到的边角）"

# ── 5. 重建被厂商文件「夹带」的通用测试辅助（同类问题：通用函数误放 vendor 文件）──
# newTestStore 定义在 internal/store/vendor_accounts_test.go，但 store_test.go 也用它。
say "5/6 重建 internal/store 的通用测试辅助"
if [ "$DRY" = 1 ]; then
  echo "    将重建 newTestStore（若缺失）"
else
  python3 - <<'PYEOF'
import pathlib
p = pathlib.Path("internal/store/store_test.go")
if p.exists():
    src = p.read_text(encoding="utf-8")
    if "func newTestStore" not in src:
        helper = '''

// newTestStore 建立临时库并返回 Store（测试结束自动清理）。
// 注意：原定义在 vendor_accounts_test.go —— 但本文件（通用测试）也用它，
// 厂商功能裁剪后必须在本文件重建，否则编译断裂（2026-09-17 实测）。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
'''
        src = src.rstrip() + helper
        p.write_text(src, encoding="utf-8")
        print("    store_test.go 已重建 newTestStore")
    else:
        print("    newTestStore 已存在，跳过")
# 确保 filepath 已导入
if "path/filepath" not in src:
    src = src.replace('import (\n\t"testing"', 'import (\n\t"path/filepath"\n\t"testing"', 1)
    p.write_text(src, encoding="utf-8")
    print("    补 filepath import")
PYEOF
fi
