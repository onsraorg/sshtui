#!/usr/bin/env bash
set -euo pipefail

# 用法: scripts/read-version.sh [VERSION_FILE]
VERSION_FILE="${1:-VERSION}"

if [ ! -f "$VERSION_FILE" ]; then
  echo "错误: 版本文件 '$VERSION_FILE' 不存在" >&2
  exit 1
fi

if [ ! -s "$VERSION_FILE" ]; then
  echo "错误: 版本文件 '$VERSION_FILE' 为空" >&2
  exit 1
fi

RAW_VERSION="$(cat "$VERSION_FILE")"

# 严格单行三段数字 X.Y.Z，首尾允许换行但内部不允许空格或非法字符
# 不支持前导零（除非单数字0），如 0.2.0 合法，01.2.0 或 0. 2.0 非法
SEMVER_REGEX='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'

# 去除首尾空白行/回车换行
VERSION="$(echo "$RAW_VERSION" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"

if [[ ! "$VERSION" =~ $SEMVER_REGEX ]]; then
  echo "错误: 版本 '$RAW_VERSION' 不是合法的 X.Y.Z semver 格式" >&2
  exit 1
fi

printf '%s\n' "$VERSION"
