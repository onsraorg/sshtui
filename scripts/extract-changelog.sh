#!/usr/bin/env bash
set -euo pipefail

# 用法: scripts/extract-changelog.sh <version> <changelog-file> <output-file>
if [ "$#" -ne 3 ]; then
  echo "用法: $0 <version> <changelog-file> <output-file>" >&2
  exit 1
fi

TARGET_VER="$1"
CHANGELOG_FILE="$2"
OUTPUT_FILE="$3"

if [ ! -f "$CHANGELOG_FILE" ]; then
  echo "错误: 更新记录文件 '$CHANGELOG_FILE' 不存在" >&2
  exit 1
fi

# 提取指定版本章节：
# 标题必须精确匹配版本且非正则前缀匹配；提取直到下一个二级标题；内容非空
TMP_NOTES="$(mktemp "${TMPDIR:-/tmp}/notes.XXXXXX")"
trap 'rm -f "$TMP_NOTES"' EXIT

FOUND=0
IN_SECTION=0

while IFS= read -r line || [ -n "$line" ]; do
  if [[ "$line" =~ ^##[[:space:]]+([^[:space:]]+)(.*)$ ]]; then
    VER_FOUND="${BASH_REMATCH[1]}"
    if [ "$IN_SECTION" -eq 1 ]; then
      # 遇到了下一个二级标题，结束提取
      break
    fi
    if [ "$VER_FOUND" = "$TARGET_VER" ]; then
      FOUND=1
      IN_SECTION=1
      continue
    fi
  elif [ "$IN_SECTION" -eq 1 ]; then
    printf '%s\n' "$line" >> "$TMP_NOTES"
  fi
done < "$CHANGELOG_FILE"

if [ "$FOUND" -ne 1 ]; then
  echo "错误: CHANGELOG.md 中未找到版本 '$TARGET_VER' 的更新说明章节" >&2
  exit 1
fi

# 检查内容是否非空（除去首尾空白/空行后必须有内容）
TRIMMED_CONTENT="$(tr -d ' \t\r\n' < "$TMP_NOTES" || true)"
if [ -z "$TRIMMED_CONTENT" ]; then
  echo "错误: CHANGELOG.md 中版本 '$TARGET_VER' 的说明内容为空" >&2
  exit 1
fi

mkdir -p "$(dirname "$OUTPUT_FILE")"
mv "$TMP_NOTES" "$OUTPUT_FILE"
trap - EXIT
