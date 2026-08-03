#!/bin/bash
# 璇玑网关编译部署脚本：带 buildDate 注入 + 部署到 supervisor
# 用法: ./deploy.sh [端口]   （默认 3002）
set -euo pipefail
cd "$(dirname "$0")"

PORT="${1:-3002}"
DATE="$(date +%Y-%m-%d)"
echo "==> 编译（buildDate=$DATE）"
go build -ldflags "-X main.buildDate=$DATE" -o /tmp/xuanji-deploy ./cmd/server

echo "==> 部署"
supervisorctl stop xuanji
cp /tmp/xuanji-deploy /data/xuanji/xuanji-server
cp cmd/server/web/admin_vue.html /data/xuanji/web/admin_vue.html
rm -f /tmp/xuanji-deploy
supervisorctl start xuanji

echo "==> 验证"
sleep 1
supervisorctl status xuanji
curl -s http://127.0.0.1:${PORT}/healthz
echo ""
echo "部署完成（buildDate=$DATE）"
