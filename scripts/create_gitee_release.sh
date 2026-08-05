#!/usr/bin/env bash
# 创建 Gitee Release 并上传 3 个二进制
# 用法: ./create_gitee_release.sh <TAG> <NAME> <NOTES_FILE>
# 示例: ./create_gitee_release.sh v1.1.1 "v1.1.1" /tmp/release_notes.md
set -e
cd /data/codes/xuanji

TOKEN="355f119665bb00afe8f47dfb0b5c3473"
REPO="icefairy/xuanji-gateway"
API="https://gitee.com/api/v5"

TAG="${1:?Usage: $0 <TAG> <NAME> <NOTES_FILE>}"
NAME="${2:?Usage: $0 <TAG> <NAME> <NOTES_FILE>}"
NOTES_FILE="${3:?Usage: $0 <TAG> <NAME> <NOTES_FILE>}"

# 关键：body 用 urllib.parse.quote() 做 URL 编码，然后用 -d "body=..." 传递
# 不要用 json.dumps（会把中文转成 \uXXXX 不可读），不要用 --data-urlencode body（Gitee 会忽略）
BODY_ENC=$(python3 -c "import urllib.parse; print(urllib.parse.quote(open('$NOTES_FILE').read()))")

echo "=== 1. 创建 Gitee Release ==="
RESP=$(curl -s -X POST "$API/repos/$REPO/releases" \
  -d "access_token=$TOKEN" \
  -d "tag_name=$TAG" \
  -d "name=$NAME" \
  -d "target_commitish=main" \
  -d "body=$BODY_ENC")
echo "$RESP" | python3 -c "
import json,sys
d = json.load(sys.stdin)
print('Release ID:', d.get('id'))
print('错误:', d.get('message', '无'))
"
RELEASE_ID=$(echo "$RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))")

if [ -z "$RELEASE_ID" ]; then
  echo "创建失败"
  exit 1
fi

echo "=== 2. 上传 3 个二进制附件 ==="
for f in dist/xuanji-server-linux-amd64.gz dist/xuanji-server-windows-amd64.exe.gz dist/xuanji-server-linux-arm64.gz; do
  echo "上传 $f ..."
  curl -s -X POST "$API/repos/$REPO/releases/$RELEASE_ID/attach_files" \
    -F "access_token=$TOKEN" \
    -F "file=@$f" | python3 -c "import json,sys; d=json.load(sys.stdin); print('  ✅', d.get('name'), d.get('size', '?'), 'bytes')"
done
echo "=== 完成 ==="
