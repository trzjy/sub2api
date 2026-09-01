#!/usr/bin/env bash
# sub2api-deploy: 拉取 GHCR 镜像并部署 sub2api 服务。
#
# 用法（在生产服务器 yiyutu-server 上）：
#   deploy.sh {tag|full-image-path}
# 示例：
#   deploy.sh 20260901-120000                        # 部署发布版（自动拼上 ghcr.io/trzjy/sub2api:）
#   deploy.sh latest                                 # 部署最新版
#   deploy.sh ghcr.io/trzjy/sub2api:20260901-120000  # 直接传完整路径（推荐，无歧义）
#
# 行为：
#   1. docker pull {image}
#   2. 更新 .env 中 SUB2API_IMAGE_TAG
#   3. 备份当前 .env 为 .env.bak-{时间戳}
#   4. docker compose up -d --force-recreate sub2api
#   5. 健康检查 /health + 前端根路径 /
#   6. 输出新容器 SHA
#
# 约束：
#   - 仅部署 sub2api 服务，Worker 镜像由 deploy-worker.sh 管理
#   - postgres/redis 不动，仅重建 sub2api 容器
#   - 执行失败时保留当前 .env.bak，不污染环境

set -euo pipefail

DEPLOY_DIR="${DEPLOY_DIR:-/opt/sub2api}"
ENV_FILE="${ENV_FILE:-$DEPLOY_DIR/.env}"
COMPOSE_FILE="${COMPOSE_FILE:-$DEPLOY_DIR/deploy-config/compose.yml}"
GHCR_IMAGE="${GHCR_IMAGE:-ghcr.io/trzjy/sub2api}"
LOCAL_ALIAS="sub2api"
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:3300}"
HEALTH_TIMEOUT="${HEALTH_TIMEOUT:-60}"

if [ $# -lt 1 ]; then
  echo "Usage: $0 {tag|full-image-path} [--skip-pull]" >&2
  echo "  tag: GHCR 镜像 tag（如 20260901-120000、latest、v1.2.3）" >&2
  echo "  --skip-pull: 跳过 docker pull（使用本地已有镜像，仅用于 dry-run / 测试）" >&2
  exit 2
fi

# 解析参数：第一个位置参数是 tag/路径，第二个可选 --skip-pull
TAG="$1"
SKIP_PULL=0
[ "${2:-}" = "--skip-pull" ] && SKIP_PULL=1
# 支持两种调用方式：
# 1. 纯 tag（如 20260901-120000）→ 自动拼上 ghcr.io/{GHCR_IMAGE} 前缀
# 2. 完整镜像路径（含 /，如 ghcr.io/trzjy/sub2api:20260901-120000）→ 直接使用
if [[ "$TAG" == *"/"* ]]; then
  REMOTE_IMAGE="$TAG"
else
  REMOTE_IMAGE="${GHCR_IMAGE:?}:${TAG}"

echo "==> 部署目标: $REMOTE_IMAGE"
echo "==> 部署目录: $DEPLOY_DIR"

# 1. 拉取镜像
if [ "$SKIP_PULL" -eq 1 ]; then
  echo "==> [1/5] 跳过 docker pull（--skip-pull）"
else
  echo "==> [1/5] 拉取镜像 $REMOTE_IMAGE ..."
  docker pull "$REMOTE_IMAGE" || {
    echo "ERROR: docker pull 失败。如果本地已有镜像，可用 --skip-pull 重试。" >&2
    exit 1
  }
fi

# 2. 备份当前 .env
if [ -f "$ENV_FILE" ]; then
  BAK="$ENV_FILE.bak-$(date +%Y%m%d-%H%M%S)"
  cp "$ENV_FILE" "$BAK"
  echo "==> [2/5] 已备份 $ENV_FILE -> $BAK"
else
  echo "==> [2/5] WARNING: $ENV_FILE 不存在，跳过备份" >&2
fi

# 3. 更新 .env 中 SUB2API_IMAGE_TAG
echo "==> [3/5] 更新 $ENV_FILE 中 SUB2API_IMAGE_TAG ..."
if [ -f "$ENV_FILE" ]; then
  if grep -qE "^SUB2API_IMAGE_TAG=" "$ENV_FILE"; then
    sed -i.bak -E "s|^SUB2API_IMAGE_TAG=.*|SUB2API_IMAGE_TAG=$REMOTE_IMAGE|" "$ENV_FILE"
  else
    echo "SUB2API_IMAGE_TAG=$REMOTE_IMAGE" >> "$ENV_FILE"
  fi
else
  echo "SUB2API_IMAGE_TAG=$REMOTE_IMAGE" > "$ENV_FILE"
fi

# 4. docker compose up
echo "==> [4/5] 重建 sub2api 容器 ..."
cd "$DEPLOY_DIR"
docker compose -f "$COMPOSE_FILE" --env-file "$ENV_FILE" up -d --force-recreate sub2api

# 5. 健康检查
echo "==> [5/5] 等待健康检查 ..."
deadline=$(( $(date +%s) + HEALTH_TIMEOUT ))
healthy=0
while [ "$(date +%s)" -lt "$deadline" ]; do
  sleep 2
  if curl -fs -o /dev/null "$HEALTH_URL/health" 2>/dev/null; then
    healthy=1
    break
  fi
done

if [ "$healthy" -ne 1 ]; then
  echo "ERROR: 健康检查超时（${HEALTH_TIMEOUT}s）" >&2
  echo "回滚提示：deploy.sh rollback 或 ./rollback.sh" >&2
  exit 1
fi

# 验证前端根路径
root_status=$(curl -s -o /dev/null -w "%{http_code}" "$HEALTH_URL/" || echo "000")
if [ "$root_status" != "200" ]; then
  echo "WARNING: 前端根路径返回 $root_status（可能 SPA 未正确嵌入）" >&2
fi

# 输出新运行信息
NEW_IMAGE_ID=$(docker inspect --format '{{.Image}}' "$(docker compose -f "$COMPOSE_FILE" --env-file "$ENV_FILE" ps -q sub2api 2>/dev/null | head -1)" 2>/dev/null || echo "unknown")
echo ""
echo "✓ 部署完成"
echo "  Image:    $REMOTE_IMAGE"
echo "  SHA256:   $NEW_IMAGE_ID"
echo "  /health:  $(curl -s -o /dev/null -w '%{http_code}' $HEALTH_URL/health)"
echo "  Frontend: HTTP $root_status"
