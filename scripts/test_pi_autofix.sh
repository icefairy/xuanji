#!/usr/bin/env bash
# pi-autofix.sh 的回归测试：验证「只取当天日志」这一关键行为。
#
# 背景（2026-10-11 实测）：脚本从**累积**日志文件里取「最后 60 行非噪音」。
# 当日志里当天只有被过滤的噪音（health check failed）时，tail -60 会回退到
# **前几日**的旧行 —— 生成的任务书与前一天逐字节相同，导致重复排查已修问题、
# 甚至重复部署同一修复。修复：先按当天日期过滤，当天无实质事件则判定"无需修复"。
#
# 用法: bash scripts/test_pi_autofix.sh
set -u
cd "$(dirname "$0")/.."

SCRIPT=./scripts/pi-autofix.sh
FAIL=0
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

check() { # check <描述> <期望> <实际>
  if [ "$2" = "$3" ]; then
    echo "  ✅ $1"
  else
    echo "  ❌ $1: 期望=$2 实际=$3"
    FAIL=1
  fi
}

# 构造测试日志：旧日期有实质事件、今天只有噪音
FIXTURE="$TMP/stderr.log"
cat > "$FIXTURE" <<'EOF'
2026/10/09 18:16:27 WARN skip arrear upstream upstream=ds model=deepseek-v4.1-flash
2026/10/09 19:19:10 WARN client disconnected, aborting retry loop model=glm-5.3-flash error="context canceled"
2026/10/10 11:46:40 WARN upstream failed, trying next upstream=gs model=glm-5.3-flash error="context canceled"
2026/10/10 20:57:21 WARN skip arrear upstream upstream=ds model=deepseek-v4.1-flash
2026/10/11 10:00:00 WARN health check failed upstream=本地ASR state=dead fails=100 reason="timeout"
2026/10/11 10:02:00 WARN health check failed upstream=本地ASR state=dead fails=101 reason="timeout"
EOF

run_extract() { # run_extract <日期> -> 打印 errors.txt 内容
  local d="$1"
  local out="$TMP/out-$d"
  AUTOFIX_ERRLOG="$FIXTURE" AUTOFIX_DATE="$d" AUTOFIX_OUT="$out" \
    bash "$SCRIPT" --dry-run >/dev/null 2>&1 || true
  cat "$out/errors.txt" 2>/dev/null
}

echo "1. 当天只有噪音（health check failed）→ 不应回退到旧日期行"
got=$(run_extract "2026/10/11" | wc -l | tr -d ' ')
check "提取行数为 0（不回退旧日期）" "0" "$got"

echo "2. 当天有实质事件 → 只取当天，不混入旧日期"
# 追加一条当天实质事件
echo '2026/10/11 11:00:00 WARN upstream failed, trying next upstream=gs model=glm-5.3-flash error="boom"' >> "$FIXTURE"
got=$(run_extract "2026/10/11")
n=$(echo "$got" | grep -c . )
check "只提取到 1 行（当天）" "1" "$n"
if echo "$got" | grep -q "2026/10/09\|2026/10/10"; then
  echo "  ❌ 混入了旧日期行"; FAIL=1
else
  echo "  ✅ 未混入旧日期行"
fi

echo "3. 指定旧日期 → 能取到该日历史行（便于人工复跑）"
got=$(run_extract "2026/10/10" | wc -l | tr -d ' ')
check "2026/10/10 提取到 2 行" "2" "$got"

echo ""
if [ "$FAIL" = "0" ]; then
  echo "全部通过 ✅"
else
  echo "存在失败 ❌"
fi
exit $FAIL
