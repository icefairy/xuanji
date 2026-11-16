#!/bin/bash
# 璇玑网关编译部署脚本：带 buildDate 注入 + 部署到 supervisor
# 用法: ./deploy.sh [端口]   （默认 3001，与 supervisor 配置一致）
# 注：stop/start 在同一脚本内相邻完成，网关只停 1~2s，不需要延迟调度。
set -euo pipefail
cd "$(dirname "$0")"

PORT="${1:-3001}"

DATE="$(date +%Y-%m-%d)"
echo "==> 编译（buildDate=$DATE）"
go build -ldflags "-X main.buildDate=$DATE" -o /tmp/xuanji-deploy ./cmd/server

echo "==> 部署"
BK="/data/xuanji/xuanji-server.bak-$(date +%Y%m%d-%H%M%S)"
cp -p /data/xuanji/xuanji-server "$BK" && echo "    备份 -> $BK"
supervisorctl stop xuanji
cp /tmp/xuanji-deploy /data/xuanji/xuanji-server
# 注意：web 静态资源已嵌入二进制（go:embed），部署目录不再保留 web/ 目录，
# 否则会遮蔽嵌入资源。改 HTML 后需重编二进制，不要 cp 到磁盘 web。
rm -f /tmp/xuanji-deploy
supervisorctl start xuanji

echo "==> 验证"
sleep 6
supervisorctl status xuanji
HZ=$(curl -s --max-time 10 http://127.0.0.1:${PORT}/healthz)
echo "$HZ"
case "$HZ" in
  *'"status":"ok"'*) echo "部署完成（buildDate=$DATE）" ;;
  *) echo "❌ healthz 异常，回滚到 $BK"
     supervisorctl stop xuanji
     cp -f "$BK" /data/xuanji/xuanji-server
     supervisorctl start xuanji
     exit 1 ;;
esac
