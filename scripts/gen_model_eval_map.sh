#!/bin/bash
# 更新璇玑管理界面「路由规则」页的评测列数据（MODEL_EVAL map）。
# 数据来自 /data/codes/model-eval 评测报告；改完需重编二进制（web 已 go:embed）。
# 用法: bash scripts/gen_model_eval_map.sh && ./deploy.sh
set -euo pipefail
cd "$(dirname "$0")/.."
/usr/bin/python3 scripts/gen_model_eval_map.py "$@"
echo "提示：HTML 已更新，执行 ./deploy.sh 重编部署生效。"
