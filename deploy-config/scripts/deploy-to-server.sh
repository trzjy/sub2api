#!/usr/bin/env bash
set -euo pipefail

# deploy-to-server.sh —— GitHub 中心化部署唯一入口（服务器端执行）
# 语义见 docs/deploy-github-flow-migration-plan.md §2.1。
# 禁区：脚本内不得出现删除类命令或同步删除工具，亦不得含任何删除逻辑。

# 1. 用法：deploy-to-server.sh [ref]，REF 取 $1，缺省 origin/main
REF="${1:-origin/main}"

# 2. 固定服务器工作目录
cd /opt/sub2api

# 3. 拉取并强制检出目标 ref（git 对 untracked 数据目录零威胁）
git fetch origin
git checkout -f "$REF"

# 3b. 迁移不可变门禁（构建之前）：阻止「已应用于生产库的迁移文件被改动」进镜像。
#     命中即 exit 1 中止部署，输出会点名被阻断的迁移文件；语义与破例开关见
#     deploy-config/scripts/predeploy-migration-guard.sh 头部注释。
./deploy-config/scripts/predeploy-migration-guard.sh "$REF"

# 4. 记录 short SHA
TAG=$(git rev-parse --short HEAD)

# 5. 构建带 -w 后缀的部署镜像
docker build -t "sub2api:${TAG}-w" .

# 6. 幂等更新同目录 .env 中 SUB2API_IMAGE_TAG（已有原地替换，无则追加；不重建整个文件；compose.yml 的 image 行自带 sub2api: 前缀，故 .env 存裸 tag 不可带前缀）
ENV_FILE="/opt/sub2api/.env"
NEW_VAL="SUB2API_IMAGE_TAG=${TAG}-w"
if grep -q '^SUB2API_IMAGE_TAG=' "$ENV_FILE"; then
  sed -i "s|^SUB2API_IMAGE_TAG=.*|${NEW_VAL}|" "$ENV_FILE"
else
  printf '%s\n' "$NEW_VAL" >> "$ENV_FILE"
fi

# 7. 用更新的镜像标签重建 sub2api 服务
docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env up -d sub2api

# 8. 后置断言（逐条执行，任一失败指名并 exit 1）

# 8a. 七个数据目录存在（minio_data 允许不存在则跳过）
DATA_DIRS=(
  data
  postgres_data
  redis_data
  minio_data
  xianyu_worker_data
  xianyu_worker_mysql
  xianyu_worker_redis
)
for d in "${DATA_DIRS[@]}"; do
  p="/opt/sub2api-data/${d}"
  if [ "$d" = "minio_data" ]; then
    [ -d "$p" ] || { echo "断言跳过(允许不存在): ${p}"; continue; }
  fi
  if [ -d "$p" ]; then
    echo "断言通过: ${p} 存在"
  else
    echo "断言失败: ${p} 不存在"
    exit 1
  fi
done

# 8b. postgres 可连，查询返回 1
PG_OK=$(docker exec sub2api-postgres psql -U sub2api -d sub2api -tAc 'select 1' 2>/dev/null || true)
if [ "$PG_OK" = "1" ]; then
  echo "断言通过: postgres select 1 == 1"
else
  echo "断言失败: postgres select 1 输出非 1 (实际: '${PG_OK}')"
  exit 1
fi

# 8c. accounts 表行数 > 0
ACCT_COUNT=$(docker exec sub2api-postgres psql -U sub2api -d sub2api -tAc 'select count(*) from accounts' 2>/dev/null || true)
if [ "${ACCT_COUNT:-0}" -gt 0 ] 2>/dev/null; then
  echo "断言通过: accounts 行数 = ${ACCT_COUNT} (>0)"
else
  echo "断言失败: accounts 行数未 > 0 (实际: '${ACCT_COUNT}')"
  exit 1
fi

# 8d. 容器健康检查 healthy（up 后先等待 start_period）
sleep 35
HEALTH=$(docker inspect sub2api --format '{{.State.Health.Status}}' 2>/dev/null || true)
if [ "$HEALTH" = "healthy" ]; then
  echo "断言通过: sub2api 容器状态 healthy"
else
  echo "断言失败: sub2api 容器健康状态非 healthy (实际: '${HEALTH}')"
  exit 1
fi

# 9. 全部通过
echo "部署成功: REF=${REF} TAG=${TAG} 镜像 sub2api:${TAG}-w"
