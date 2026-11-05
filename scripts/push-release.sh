#!/usr/bin/env bash
set -euo pipefail

# 用法: scripts/push-release.sh <tag> <branch>
if [ "$#" -ne 2 ]; then
  echo "用法: $0 <tag> <branch>" >&2
  exit 1
fi

TAG="$1"
BRANCH="$2"

# 检查远端是否已存在该 tag。不能吞掉 ls-remote 失败（网络/权限错误直接报错退出）
REMOTE_TAG_CHECK="$(git ls-remote --tags origin "refs/tags/$TAG")"
if [ -n "$REMOTE_TAG_CHECK" ]; then
  echo "错误: 远端已存在标签 '$TAG'，禁止覆盖已发布的 tag" >&2
  exit 1
fi

# 检查本地 tag 状态：禁止重用不符 HEAD 的旧 tag
CREATED_TAG_LOCALLY=0
if git rev-parse -q --verify "refs/tags/$TAG" >/dev/null; then
  # 本地 tag 已存在，核验是否指向当前 HEAD
  TAG_COMMIT="$(git rev-parse "refs/tags/$TAG^{commit}")"
  HEAD_COMMIT="$(git rev-parse HEAD)"
  if [ "$TAG_COMMIT" != "$HEAD_COMMIT" ]; then
    echo "错误: 本地已存在标签 '$TAG' 但未指向当前 HEAD，禁止重用旧标签" >&2
    exit 1
  fi
else
  VERSION="${TAG#v}"
  if ! git tag -a "$TAG" -m "sshtui $VERSION"; then
    echo "错误: 创建本地 tag '$TAG' 失败" >&2
    exit 1
  fi
  CREATED_TAG_LOCALLY=1
fi

# 明确推送分支与 tag: HEAD:refs/heads/<branch> refs/tags/<tag>
PUSH_FAILED=0
if ! git push --atomic origin "HEAD:refs/heads/$BRANCH" "refs/tags/$TAG"; then
  PUSH_FAILED=1
fi

if [ "$PUSH_FAILED" -eq 1 ]; then
  echo "警告: git push 失败，正在核实远端标签状态..." >&2

  # 核实远端 tag 是否存在或网络是否无法连接
  REMOTE_TAG_OUT=""
  if REMOTE_TAG_OUT="$(git ls-remote --tags origin "refs/tags/$TAG" 2>/dev/null)"; then
    if [ -n "$REMOTE_TAG_OUT" ]; then
      echo "提示: 远端已存在标签 '$TAG'（可能已完成推送或此前已发布）。保留本地 tag，请人工核查远端状态！" >&2
      exit 1
    else
      if [ "$CREATED_TAG_LOCALLY" -eq 1 ]; then
        echo "提示: 远端未创建标签 '$TAG'，安全清理本次新建的本地 tag..." >&2
        git tag -d "$TAG" >/dev/null || true
      else
        echo "提示: 远端未创建标签 '$TAG'，但该 tag 在本次运行前已存在于本地，不予删除。" >&2
      fi
      exit 1
    fi
  else
    echo "警告: 无法核实远端标签状态。为避免误删已发布的远端标签，已保留本地 tag，请网络恢复后人工核查！" >&2
    exit 1
  fi
fi

echo "✓ 成功发布并推送分支 '$BRANCH' 与标签 '$TAG'"
