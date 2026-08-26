#!/usr/bin/env bash
# 璇玑网关自动排障：提取最新错误日志 → pi 排查源码修复 → 编译部署 → 输出结论
# 用法: ./scripts/pi-autofix.sh [--dry-run]
# 流程: 1) 抓 supervisor 最新 xuanji stderr 错误  2) 生成任务书  3) pi -p 修复+部署  4) 打印结果
set -u
cd "$(dirname "$0")/.." # /data/codes/xuanji

TS=$(date +%Y%m%d-%H%M%S)
OUT="/tmp/xuanji-autofix-$TS"
mkdir -p "$OUT"

# ---- 1. 提取最新错误日志 ----
ERRLOG=$(ls -t /var/log/supervisor/xuanji-stderr*.log 2>/dev/null | grep -v '\.1$' | head -1)
if [ -z "$ERRLOG" ]; then
  echo "[autofix] 未找到 xuanji stderr 日志"
  exit 1
fi
echo "[autofix] 日志源: $ERRLOG"

# 优先 ERROR/panic/fatal（真问题）；没有再退回 WARN 尾部（可能是上游噪音）
grep -aE '\b(ERROR|panic|fatal)\b' "$ERRLOG" | tail -60 > "$OUT/errors.txt" || true
if [ ! -s "$OUT/errors.txt" ]; then
  grep -aE '\bWARN\b' "$ERRLOG" | tail -30 > "$OUT/errors.txt" || true
fi
if [ ! -s "$OUT/errors.txt" ]; then
  echo "[autofix] 最近日志无错误，无需修复 ($(date '+%F %T'))"
  exit 0
fi
cp "$OUT/errors.txt" "$OUT/errors-recent.txt"
echo "[autofix] 提取到 $(wc -l < "$OUT/errors.txt") 行错误样本"

# ---- 2. 生成 pi 任务书（分段拼接，避免 heredoc 展开/反引号风险）----
{
  echo "# 任务：根据璇玑网关最新错误日志排查并修复源码"
  echo ""
  echo "你是璇玑网关(Go, /data/codes/xuanji)的维护工程师。以下是 supervisor 采集的最新错误样本"
  echo "(可能混有上游侧噪音，如 429 限流/免费额度耗尽)："
  echo ""
  echo '```'
  cat "$OUT/errors-recent.txt"
  echo '```'
  echo ""
  echo "## 要求"
  echo "1. 先分类：哪些是网关自身 bug，哪些是上游/外部问题(429限流、模型不存在、额度耗尽等)。只修网关自身 bug。"
  echo "2. 对确认的 bug：最小侵入修复(不改无关文件)，补/改单测覆盖。"
  echo "3. 验证：go build ./... 与相关包 go test 必须全绿才允许进入下一步。"
  echo "4. 全绿后部署(必须按此顺序，运行中的二进制不可直接覆盖)："
  echo "   ./scripts/build.sh && supervisorctl stop xuanji && cp xuanji-server /data/xuanji/xuanji-server && supervisorctl start xuanji"
  echo "   然后 curl http://127.0.0.1:3002/healthz 确认 status ok 且 build_date 为今天。"
  echo "5. 若 healthz 异常：用 ls -t /data/xuanji/xuanji-server.bak-* 最新备份回滚(先 stop 再 cp 再 start)，并在结论中说明。"
  echo "6. 上游/外部问题不要改代码，在结论中列出即可。"
  echo "7. 最后输出中文总结：发现的问题 / 根因 / 改动文件 / 测试结果 / 部署结果(或未部署原因)。"
  echo ""
  echo "## 禁止"
  echo "- git push、git rebase、git reset --hard"
  echo "- 删除数据文件、修改 /etc/supervisor、动 xuanji.db"
  echo "- 跳过测试直接部署"
} > "$OUT/task.md"
echo "[autofix] 任务书: $OUT/task.md ($(wc -c < "$OUT/task.md") 字节)"

# ---- 3. dry-run 支持 ----
if [ "${1:-}" = "--dry-run" ]; then
  echo "[autofix] DRY-RUN 模式，仅生成任务书，未调用 pi"
  exit 0
fi

# ---- 4. 执行 pi（前台 -p，复用会话便于连续排查）----
pi -p \
  --session-id xuanji-autofix \
  --append-system-prompt "你是资深 Go 工程师，负责璇玑网关维护。严格按任务书执行：谨慎最小修改、测试先行、部署必须验证 healthz。" \
  @"$OUT/task.md"
RC=$?
echo ""
echo "[autofix] pi 退出码=$RC 产物目录=$OUT 完成=$(date '+%F %T')"
exit $RC
