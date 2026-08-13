#!/usr/bin/env bash
# 创建 GitHub Release 并上传 3 个二进制
# 用法: ./create_github_release.sh <TAG> <NAME> <NOTES_FILE>
# 示例: ./create_github_release.sh v1.1.1 "v1.1.1" /tmp/release_notes.md
set -e
cd /data/codes/xuanji

# GitHub Token 从环境变量读取（不要硬编码进脚本，避免 GitHub Push Protection 拦截）
TOKEN="${GITHUB_TOKEN:?请先 export GITHUB_TOKEN=ghp_xxx}"
PROXY="socks5h://127.0.0.1:10809"
REPO="icefairy/xuanji"

TAG="${1:?Usage: $0 <TAG> <NAME> <NOTES_FILE>}"
NAME="${2:?Usage: $0 <TAG> <NAME> <NOTES_FILE>}"
NOTES_FILE="${3:?Usage: $0 <TAG> <NAME> <NOTES_FILE>}"

BODY=$(python3 -c "import json; print(json.dumps(open('$NOTES_FILE').read()))")

echo "=== 1. 创建 GitHub Release ==="
RELEASE_JSON=$(curl -s -x "$PROXY" \
  -X POST "https://api.github.com/repos/$REPO/releases" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Accept: application/vnd.github+json" \
  -H "Content-Type: application/json" \
  -d "{
    \"tag_name\": \"$TAG\",
    \"name\": \"$NAME\",
    \"body\": $BODY,
    \"draft\": false,
    \"prerelease\": false,
    \"generate_release_notes\": false
  }")
echo "$RELEASE_JSON" | python3 -c "import json,sys; d=json.load(sys.stdin); print('Release ID:', d.get('id')); print('URL:', d.get('html_url')); print('错误:', d.get('message', '无'))"
RELEASE_ID=$(echo "$RELEASE_JSON" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))")

if [ -z "$RELEASE_ID" ]; then
  echo "创建失败"
  exit 1
fi

echo "=== 2. 上传 3 个二进制资产 ==="
for f in dist/xuanji-server-linux-amd64.gz dist/xuanji-server-windows-amd64.exe.gz dist/xuanji-server-linux-arm64.gz; do
  echo "上传 $f ..."
  curl -s -x "$PROXY" \
    -X POST "https://uploads.github.com/repos/$REPO/releases/$RELEASE_ID/assets?name=$(basename $f)" \
    -H "Authorization: Bearer $TOKEN" \
    -H "Accept: application/vnd.github+json" \
    -H "Content-Type: application/gzip" \
    --data-binary "@$f" | python3 -c "import json,sys; d=json.load(sys.stdin); print('  ✅', d.get('name'), d.get('size', '?' ), 'bytes')"
done
echo "=== 完成 ==="