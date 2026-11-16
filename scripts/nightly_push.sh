#!/usr/bin/env bash
# 璇玑网关 nightly 自动提交推送脚本（每晚 20:00 由 crontab 触发）：
#   1) 提交未提交/未推送的代码改动
#   3) 推送 public 与 main 到 origin（自建 Gitea，局域网直连；github/gitee 不自动推送）
#
# 注意：不再把 public 合并到 main。public 是 main 的裁剪副本（移除厂商账号池等私有功能），
# 正确流向是 main → public（裁剪生成），反向合并只会把 main 的生产代码删掉。
set -uo pipefail
export GIT_SSH_COMMAND="ssh -o ServerAliveInterval=30 -o ServerAliveCountMax=3 -o ConnectTimeout=20"

REPO=/data/codes/xuanji
LOG=/var/log/xuanji-nightly.log
ts() { date '+%Y-%m-%d %H:%M:%S'; }

{
  echo "==== $(ts) START ===="
  cd "$REPO" || { echo "cd $REPO failed"; exit 1; }

  # 记录进入时的分支：本脚本只是替用户做提交与推送，不应改变工作区的检出状态
  # （否则用户下次打开终端会莫名停在 public，甚至在错误的分支上继续工作）。
  orig_branch=$(git symbolic-ref --quiet --short HEAD || echo "")

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

  # 2) 推送 origin（public 与 main）
  # 注意：只推 origin（自建 Gitea，局域网地址，绕开全局 socks 代理直连）。
  # github / gitee 不再自动推送——需要更新时必须人工明确要求。
  # 显式 refspec，避免 push 配置把 public 误推到 main 导致 public 分支从不更新。
  for b in main public; do
    for r in origin; do
      echo "  -- push $b -> $r"
      extra=(-c http.proxy= -c https.proxy=)
      timeout 300 git "${extra[@]}" push "$r" "refs/heads/$b:refs/heads/$b" 2>&1 && continue
      # 瞬时故障（408/断流）重试一次
      echo "     retry $b -> $r"
      sleep 3
      timeout 300 git "${extra[@]}" push "$r" "refs/heads/$b:refs/heads/$b" 2>&1
    done
  done

  # 切回进入时的分支（分支可能已被删除，故先校验存在）
  if [ -n "$orig_branch" ] && git show-ref --verify --quiet "refs/heads/$orig_branch"; then
    if [ "$(git symbolic-ref --quiet --short HEAD)" != "$orig_branch" ]; then
      git checkout "$orig_branch" 2>&1 || echo "checkout $orig_branch failed"
    else
      echo "  -> 工作分支已是 $orig_branch"
    fi
  else
    if [ -z "$orig_branch" ]; then
      echo "  -> 进入时无分支（detached HEAD），保持现状"
    else
      echo "  -> 进入时分支 $orig_branch 已不存在（可能在脚本运行中被删），保持现状"
    fi
  fi

  echo "==== $(ts) END ===="
} >> "$LOG" 2>&1
