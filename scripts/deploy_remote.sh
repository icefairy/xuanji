#!/bin/bash
# 璇玑网关「原子远程部署」脚本：在 194 服务器上停→替换→起→验证，失败自动回滚上一版本。
# 用法: ./scripts/deploy_remote.sh [二进制路径] [目标服务器 ssh 别名/用户@host] [ssh 端口] [端口]
#
# 示例:
#   ./scripts/deploy_remote.sh /tmp/xuanji-deploy icefairy@10.1.11.194 3022 3002
#
# 关键点（踩坑记录）:
#   - 必须先 stop 再 cp：服务运行中替换二进制会报 "Text file busy"，导致服务中断且没换成功。
#   - 整个流程放在一台远程主机的单个 bash -s 里原子执行，避免本机多次 ssh 断点造成
#     已 stop 但未 start 的真空期（曾因分步操作中断服务，需人工恢复）。
#   - start 后做健康检查：失败则回滚到备份二进制并重新拉起，保证不把坏版本留在线。
set -euo pipefail

BIN_SRC="${1:?用法: $0 <二进制路径> [host] [ssh端口] [服务端口]}"
HOST="${2:-icefairy@10.1.11.194}"
SSH_PORT="${3:-3022}"
SVC_PORT="${4:-3002}"
APP_DIR="/data/xjwg"
SVC_BIN="${APP_DIR}/xuanji-server"
SDIR="$(cd "$(dirname "$0")/.." && pwd)"

echo "==> [1/3] 本地校验 + 上传二进制到远程 /tmp"
test -x "$BIN_SRC" || { echo "❌ 二进制不存在或不可执行: $BIN_SRC"; exit 1; }
DATE_HASH="$(md5sum "$BIN_SRC" | awk '{print $1}')"
echo "    本地 md5 = $DATE_HASH"
timeout 120 scp -o StrictHostKeyChecking=no -P "$SSH_PORT" -O "$BIN_SRC" "${HOST}:/tmp/xuanji-deploy-new"
timeout 40 ssh -p "$SSH_PORT" "$HOST" "test -f /tmp/xuanji-deploy-new && echo '    远端已收到'"

echo "==> [2/3] 远端原子替换 + 启动 + 健康检查（失败自动回滚）"
timeout 120 ssh -p "$SSH_PORT" "$HOST" "sudo bash -s" <<REMOTE_SCRIPT
set -euo pipefail
SRC=/tmp/xuanji-deploy-new
SVC_BIN=${SVC_BIN}
APP_DIR=${APP_DIR}
SVC_PORT=${SVC_PORT}
REMOTE_HASH=\$(md5sum \$SRC | awk '{print \$1}')
if [ "\$REMOTE_HASH" != "${DATE_HASH}" ]; then echo "❌ 远端 md5 不一致: \$REMOTE_HASH"; exit 1; fi
echo "    远端 md5 一致: \$REMOTE_HASH"

TS=\$(date +%Y%m%d-%H%M%S)
BK=\${APP_DIR}/xuanji-server.bak.\${TS}
cp \$SVC_BIN \$BK
echo "    备份旧版 -> \$BK"

echo "    停止服务"
supervisorctl stop xuanji || true
cp \$SRC \$SVC_BIN
chown icefairy:icefairy \$SVC_BIN
chmod +x \$SVC_BIN

echo "    启动服务"
supervisorctl start xuanji
sleep 2
supervisorctl status xuanji

HEALTH=\$(curl -s http://127.0.0.1:\$SVC_PORT/healthz || true)
if echo "\$HEALTH" | grep -q '"status":"ok"'; then
    echo "    ✅ 健康检查通过: \$HEALTH"
else
    echo "    ⚠️ 健康检查失败: \$HEALTH -> 回滚到 \$BK"
    supervisorctl stop xuanji || true
    cp \$BK \$SVC_BIN
    chown icefairy:icefairy \$SVC_BIN
    supervisorctl start xuanji
    echo "    回滚完成"
    exit 2
fi
rm -f \$SRC
echo "    ✅ 部署完成"
REMOTE_SCRIPT

echo "==> [3/3] 确认远端服务状态"
timeout 40 ssh -p "$SSH_PORT" "$HOST" "sudo supervisorctl status xuanji; curl -s http://127.0.0.1:\$SVC_PORT/healthz; echo"
echo "==> 全部完成 ✔"