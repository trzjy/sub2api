#!/usr/bin/env bash
# sub2api-rollback: 从 .env.bak-* 历史备份回滚到上一（多个）版本。
#
# 用法（在生产服务器 yiyutu-server 上）：
#   rollback.sh          # 回滚到上一个 tag
#   rollback.sh N        # 回滚 N 个版本（默认 1）
#
# 依赖：deploy.sh 使用相同的 .env.bak-{timestamp} 备份命名
# 约束：KEEP_BAKS（默认 10）个以内的备份可回滚，超出则失败

set -euo pipefail

DEPLOY_DIR="${DEPLOY_DIR:-/opt/sub2api}"
ENV_FILE="${ENV_FILE:-$DEPLOY_DIR/.env}"
KEEP_BAKS="${KEEP_BAKS:-10}"

if [ $# -gt 1 ]; then
  echo "Usage: $0 [N]" >&2
  echo "  N: 回滚版本数（默认 1）" >&2
  exit 2
fi

N="${1:-1}"

# 收集所有 .env.bak-* 按文件名逆序（即最新在前）
mapfile -t BAKS < <(ls -1 "$ENV_FILE.bak-"* 2>/dev/null | sort -r)

if [ ${#BAKS[@]} -eq 0 ]; then
  echo "ERROR: 找不到任何 $ENV_FILE.bak-* 备份，无法回滚" >&2
  echo "提示：请确认 deploy.sh 已执行过至少一次" >&2
  exit 1
fi

if [ "$N" -gt ${#BAKS[@]} ]; then
  echo "ERROR: 请求回滚 $N 个版本，但只有 ${#BAKS[@]} 个备份" >&2
  exit 1
fi

# 获取目标备份（新部署的备份在列表第一个，目标是第 N 个之后的）
TARGET_BAK="${BAKS[$((N))]}"
if [ -z "$TARGET_BAK" ] || [ ! -f "$TARGET_BAK" ]; then
  echo "ERROR: 目标备份 $TARGET_BAK 不存在" >&2
  exit 1
fi

echo "==> 回滚 $N 个版本，目标备份: $TARGET_BAK"
grep -E "^SUB2API_IMAGE_TAG=" "$TARGET_BAK" | sed 's/^/    /'

# 备份当前 .env（当前版本也要保留）
CURRENT_BAK="$ENV_FILE.bak-rollback-before-$(date +%Y%m%d-%H%M%S)"
cp "$ENV_FILE" "$CURRENT_BAK"
echo "==> 当前 .env 已备份至 $CURRENT_BAK"

# 从目标备份恢复 .env
cp "$TARGET_BAK" "$ENV_FILE"
echo "==> 已从 $TARGET_BAK 恢复 $ENV_FILE"

# 解析 tag 并执行 deploy（复用 deploy.sh 的逻辑，但只需 pull + recreate）
TAG_VALUE=$(grep "^SUB2API_IMAGE_TAG=" "$ENV_FILE" | cut -d= -f2-)
echo "==> 从备份提取 tag: $TAG_VALUE"

# 提取纯 tag（去掉 ghcr.io/{owner}/sub2api: 前缀）
PURE_TAG="${TAG_VALUE##*:}"
# 如果不是纯 tag 格式（不含 /），再检查是否有 registry 前缀需要去除
if [[ "$PURE_TAG" == *"/"* ]]; then
  # 完整路径如 ghcr.io/trzjy/sub2api:20260901-120000
  # 只取冒号后的部分
  PURE_TAG="${PURE_TAG##*:}"
fi

echo "==> 拉取镜像 ..."
docker pull "$TAG_VALUE" 2>/dev/null || {
  echo "WARNING: docker pull 失败，尝试使用本地已有镜像" >&2
}

echo "==> 重建 sub2api 容器 ..."
cd "$DEPLOY_DIR"
docker compose -f deploy-config/compose.yml --env-file "$ENV_FILE" up -d --force-recreate sub2api

sleep 10

# 健康检查
if curl -fs -o /dev/null http://127.0.0.1:3300/health 2>/dev/null; then
  echo ""
  echo "✓ 回滚完成"
  echo "  恢复自: $TARGET_BAK"
  echo "  Image:  $TAG_VALUE"
else
  echo ""
  echo "WARNING: /health 未通过，当前可能仍有问题"
fi
