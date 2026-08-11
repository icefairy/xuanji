#!/usr/bin/env bash
# 构建璇玑网关闭门二进制：注入 buildDate 版本号（替代手写 -ldflags）
# 用法: ./scripts/build.sh [VERSION]  默认 VERSION=当天日期 2026-08-11
# 产物: xuanji-server（当前目录）
set -e
cd "$(dirname "$0")/.."

VERSION="${1:-$(date +%Y-%m-%d)}"
echo "=== buildDate=$VERSION ==="
go build -ldflags "-X main.buildDate=$VERSION" -o xuanji-server ./cmd/server
echo "=== BUILD OK ==="
ls -la xuanji-server
echo "验证: ./xuanji-server --version 或 curl http://127.0.0.1:3002/healthz 看 build_date"