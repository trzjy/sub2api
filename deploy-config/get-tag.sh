#!/usr/bin/env bash
# sub2api-get-tag: 输出当前运行的 sub2api 镜像 tag 和 SHA256。
#
# 用法（在生产服务器 yiyutu-server 上）：
#   ./get-tag.sh
#
# 输出示例：
#   sub2api:20260901-120000  (sha256:ab8770bd...)

set -euo pipefail

DEPLOY_DIR="${DEPLOY_DIR:-/opt/sub2api}"
ENV_FILE="${ENV_FILE:-$DEPLOY_DIR/.env}"

TAG_VALUE=$(grep "^SUB2API_IMAGE_TAG=" "$ENV_FILE" 2>/dev/null | cut -d= -f2-)
if [ -z "$TAG_VALUE" ]; then
  echo "ERROR: $ENV_FILE 中找不到 SUB2API_IMAGE_TAG" >&2
  exit 1
fi

# 获取运行中容器的镜像 ID（short）
CONTAINER_ID=$(docker compose -f "$DEPLOY_DIR/deploy-config/compose.yml" --env-file "$ENV_FILE" ps -q sub2api 2>/dev/null | head -1)
if [ -z "$CONTAINER_ID" ]; then
  echo "ERROR: sub2api 容器未运行" >&2
  exit 1
fi

IMAGE_ID=$(docker inspect --format '{{.Image}}' "$CONTAINER_ID" 2>/dev/null)
SHORT_SHA="${IMAGE_ID#sha256:}"
SHORT_SHA="${SHORT_SHA:0:16}"

echo "$TAG_VALUE  (sha256:${SHORT_SHA}...)"
