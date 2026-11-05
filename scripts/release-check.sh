#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VERSION="$("$SCRIPT_DIR/read-version.sh" VERSION)"

# 检查当前分支，拒绝游离 HEAD (detached HEAD)
BRANCH="$(git symbolic-ref --quiet --short HEAD 2>/dev/null || true)"
if [ -z "$BRANCH" ]; then
  echo "错误: 当前处于游离 HEAD (detached HEAD) 状态，无法发布" >&2
  exit 1
fi

# 获取远端 origin 默认分支，必须成功查询到，无法查询时直接失败（不猜、不回退本地缓存）
SYMREF_OUT="$(git ls-remote --symref origin HEAD)"
DEFAULT_BRANCH="$(printf '%s\n' "$SYMREF_OUT" | awk '/^ref: refs\/heads\//{ sub("ref: refs/heads/", ""); print $1; exit }')"

if [ -z "$DEFAULT_BRANCH" ]; then
  echo "错误: 无法从 origin 获取默认分支" >&2
  exit 1
fi

if [ "$BRANCH" != "$DEFAULT_BRANCH" ]; then
  echo "错误: 当前分支 '$BRANCH' 不是 origin 默认分支 '$DEFAULT_BRANCH'，无法发布" >&2
  exit 1
fi

echo "OK: 分支 $BRANCH == 默认分支 $DEFAULT_BRANCH, 版本 $VERSION"
