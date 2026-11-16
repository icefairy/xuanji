#!/usr/bin/env bash
# 璇玑网关 nightly 自动提交推送脚本（每晚 20:00 由 crontab 触发）：
#   1) 提交未提交/未推送的代码改动
#   2) 合并 public 分支到 main（历史惯例 "public 合入 main"）
#   3) 推送 public 与 main 到所有远端（gitea 关口代直连局域网；gitee 走代理；github 走 SSH）
set -uo pipefail

REPO=/data/codes/xuanji
LOG=/var/log/xuanji-nightly.log
ts() { date '+%Y-%m-%d %H:%M:%S'; }

{
  echo "==== $(ts) START ===="
  cd "$REPO" || { echo "cd $REPO failed"; exit 1; }

  # 工作分支固定为 public
  git checkout public 2>&1 || echo "checkout public failed"

  # 1) 提交未提交改动（若有）
  if [ -n "$(git status --porcelain)" ]; then
    git add -A
    git commit -m "chore: auto-commit pending changes at $(date '+%Y-%m-%d %H:%M')" 2>&1 \
      && echo "  -> committed pending changes" || echo "  -> commit failed"
  else
    echo "  -> no pending changes"
  fi

  # 2) 合并 public 到 main
  git checkout main 2>&1 || echo "checkout main failed"
  if git merge public --no-edit 2>&1; then
    echo "  -> merged public into main"
  else
    echo "  -> merge conflict on main, aborting (public already pushed separately)"
    git merge --abort 2>&1
  fi

  # 3) 推送所有远端（public 与 main）
  for b in main public; do
    for r in gitea gitee github; do
      echo "  -- push $b -> $r"
      extra=()
      # gitea 为局域网地址，绕开全局 socks 代理直连
      [ "$r" = "gitea" ] && extra=(-c http.proxy= -c https.proxy=)
      # 显式 refspec，避免 gitee/github 的 push 配置(public→main)把 
      # public 误推到 main 导致 public 分支从不更新
      git "${extra[@]}" push "$r" "refs/heads/$b:refs/heads/$b" 2>&1 && continue
      # 瞬时故障（408/断流）重试一次
      echo "     retry $b -> $r"
      sleep 3
      git "${extra[@]}" push "$r" "refs/heads/$b:refs/heads/$b" 2>&1
    done
  done

  # 回到 public 工作分支
  git checkout public 2>&1
  echo "==== $(ts) END ===="
} >> "$LOG" 2>&1
