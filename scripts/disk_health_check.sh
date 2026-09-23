#!/usr/bin/env bash
# 每日巡检 sda（Seagate SkyHawk 4T）的 UDMA_CRC_Error_Count 与 dmesg 新增接口错误
# 用法：bash scripts/disk_health_check.sh
# 适合放入 crontab：0 6 * * * /path/to/disk_health_check.sh

set -euo pipefail

DEV="/dev/sda"
LOG="${HOME}/.disk_health.log"
CRIT_THRESHOLD=12   # 超过此值说明仍在累积 CRC 错误

ts() { date '+%F %T'; }

crc=$(smartctl -A "$DEV" 2>/dev/null \
  | awk '/^199.*UDMA_CRC_Error_Count/ {print $10}' || echo "?")

# 读上一次的值
prev_file="${HOME}/.disk_health_last"
prev=$(cat "$prev_file" 2>/dev/null || echo "")

echo "$(ts) sda UDMA_CRC=${crc} prev=${prev:-N/A}" >> "$LOG"
echo "$crc" > "$prev_file"

# 检查 dmesg 最近 1 天有无新 ata1 接口错误（dmesg -T 行首形如 [Thu Sep 23 13:40:00 2026]）
last_err_ts=$(dmesg -T 2>/dev/null | grep 'irq_stat' | tail -1 | grep -oE '\[[A-Za-z]+ [A-Za-z]+ [0-9]+ [0-9:]+ [0-9]{4}\]' | tr -d '[]')
if [ -n "$last_err_ts" ]; then
  last_err_epoch=$(date -d "$last_err_ts" +%s 2>/dev/null || echo 0)
  day_ago_epoch=$(( $(date +%s) - 86400 ))
  if [ "$last_err_epoch" -ge "$day_ago_epoch" ]; then
    echo "ALERT: 24h 内有新的 ata 接口错误：[${last_err_ts}]" >> "$LOG"
  fi
fi

if [ "$crc" != "?" ] && [ "$crc" -ge "$CRIT_THRESHOLD" ]; then
  echo "ALERT: CRC 计数 ${crc} >= ${CRIT_THRESHOLD}，请检查 SATA 线缆" >> "$LOG"
  # 可选：发通知（按需启用）
  # notify-send "磁盘 CRC 告警" "sda UDMA_CRC=${crc}，请检查线缆"
fi

echo "done crc=${crc}"
