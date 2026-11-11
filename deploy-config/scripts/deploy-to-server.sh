#!/usr/bin/env bash
set -euo pipefail

# deploy-to-server.sh —— GitHub 中心化部署唯一入口（服务器端执行）
# 语义见 docs-local/deploy-github-flow-migration-plan.md §2.1。
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
#
#     必须优先调用「已安装到 /usr/local/sbin 的稳定绝对路径」：第 3 步已把工作树
#     checkout 到目标 ref，若该 ref 早于门禁脚本的引入，仓库内脚本会随工作树一起
#     消失（实测：回滚到旧提交时 exit 127 "No such file or directory"）。而回滚正是
#     事故止血的标准动作，门禁必须在任何 ref 下都可用，故照 sub2api-clean-releases
#     的惯例安装到 /usr/local/sbin。两者都缺失时 fail-closed 阻断 —— 门禁缺失即等于
#     没有保护，与门禁内部「postgres 不可连则阻断」的取向一致。
GUARD_INSTALLED="/usr/local/sbin/sub2api-migration-guard"
GUARD_IN_REPO="./deploy-config/scripts/predeploy-migration-guard.sh"

if [ -x "$GUARD_INSTALLED" ]; then
  "$GUARD_INSTALLED" "$REF"
elif [ -x "$GUARD_IN_REPO" ]; then
  "$GUARD_IN_REPO" "$REF"
else
  echo "===================================================================="
  echo "阻断 · 迁移不可变门禁不可用：${GUARD_INSTALLED} 与 ${GUARD_IN_REPO} 均缺失"
  echo "===================================================================="
  echo "门禁缺失 = 没有保护，按 fail-closed 中止部署（构建之前）。"
  echo "一分钟恢复（安装版门禁，与回滚场景兼容）："
  echo "  cd /opt/sub2api && git fetch origin && git checkout -f origin/main"
  echo "  install -m 0755 deploy-config/scripts/predeploy-migration-guard.sh ${GUARD_INSTALLED}"
  exit 1
fi

# 4. 记录 short SHA
TAG=$(git rev-parse --short HEAD)

# 5. 构建带 -w 后缀的部署镜像（sub2api 主镜像 + web-quota-worker sidecar 镜像，
#    两者同 tag，避免宿主机残留旧 web-quota-worker:local 导致 sidecar 继续跑旧代码）
#
#    幂等（2026-11-12 修）：同一 ref 重跑时同名镜像已存在会让 buildx 导出阶段报
#    "image ... already exists" 而整个部署失败。改为**先构建到临时 tag、成功后再改
#    正式 tag**：既不撞名，失败时旧镜像仍在（可回滚），也不需要预先删除镜像。
build_image_idempotent() {
  # 注意：tmp 必须用 $1 拼，不能用同行的 ${target}——set -u 下同行 local 赋值按顺序生效，
  # ${target} 此时尚未赋值，会报 "target: unbound variable"（2026-11-12 实测）。
  local target="$1" ctx="$2" built=0
  local tmp="${1}-building"
  # 先清掉可能残留的临时 tag（上次失败留下的）。
  docker rmi "$tmp" >/dev/null 2>&1 || true
  if docker build -t "$tmp" "$ctx"; then
    # 构建成功：把临时 tag 改名为正式 tag（覆盖同名旧镜像）。
    docker tag "$tmp" "$target" && docker rmi "$tmp" >/dev/null 2>&1 || true
    built=1
  else
    docker rmi "$tmp" >/dev/null 2>&1 || true
    echo "[$(date '+%F %T')] ERROR: 镜像构建失败: ${target}（旧镜像保留，未做变更）" >&2
  fi
  return $((1 - built))
}

build_image_idempotent "sub2api:${TAG}-w" "." || { echo "[$(date '+%F %T')] ERROR: sub2api 主镜像构建失败" >&2; exit 1; }
build_image_idempotent "web-quota-worker:${TAG}-w" "./deploy-config/web-quota-worker" || { echo "[$(date '+%F %T')] ERROR: web-quota-worker 镜像构建失败" >&2; exit 1; }

# 6. 幂等更新同目录 .env 中 SUB2API_IMAGE_TAG / WEB_QUOTA_WORKER_IMAGE_TAG（已有原地替换，无则追加；不重建整个文件；compose.yml 的 image 行自带 sub2api: 前缀，故 .env 存裸 tag 不可带前缀）
ENV_FILE="/opt/sub2api/.env"
NEW_VAL="SUB2API_IMAGE_TAG=${TAG}-w"
if grep -q '^SUB2API_IMAGE_TAG=' "$ENV_FILE"; then
  sed -i "s|^SUB2API_IMAGE_TAG=.*|${NEW_VAL}|" "$ENV_FILE"
else
  printf '%s\n' "$NEW_VAL" >> "$ENV_FILE"
fi

NEW_WQ_VAL="WEB_QUOTA_WORKER_IMAGE_TAG=${TAG}-w"
if grep -q '^WEB_QUOTA_WORKER_IMAGE_TAG=' "$ENV_FILE"; then
  sed -i "s|^WEB_QUOTA_WORKER_IMAGE_TAG=.*|${NEW_WQ_VAL}|" "$ENV_FILE"
else
  printf '%s\n' "$NEW_WQ_VAL" >> "$ENV_FILE"
fi

# 7. 用更新的镜像标签重建服务（必须全项目 up -d：本项目含 xianyu-worker 等共享 xianyu-internal 网络的服务，单服务 up 会触发网络重建失败导致服务零容器，见 sub2api-ops.md §7.12）
docker compose -f deploy-config/compose.yml --env-file /opt/sub2api/.env up -d

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

# 8d. 容器健康检查 healthy（有界轮询：最多 24 次 × 5s，总上限 120s）
#     web-quota-worker start_period 20s，且 sub2api depends_on 它 service_healthy 后才启动
#     （sub2api 自身 start_period 30s），固定 sleep 必然在 sub2api 仍 starting 时误判失败，
#     故用轮询等到两容器同时 healthy；命中 unhealthy 立即退出轮询走下方失败分支。
HEALTH=""
WQ_HEALTH=""
for ((i = 0; i < 24; i++)); do
  HEALTH=$(docker inspect sub2api --format '{{.State.Health.Status}}' 2>/dev/null || true)
  WQ_HEALTH=$(docker inspect web-quota-worker --format '{{.State.Health.Status}}' 2>/dev/null || true)
  if [ "$HEALTH" = "healthy" ] && [ "$WQ_HEALTH" = "healthy" ]; then
    break
  fi
  if [ "$HEALTH" = "unhealthy" ] || [ "$WQ_HEALTH" = "unhealthy" ]; then
    break
  fi
  sleep 5
done

if [ "$HEALTH" = "healthy" ]; then
  echo "断言通过: sub2api 容器状态 healthy"
else
  echo "断言失败: sub2api 容器健康状态非 healthy (实际: '${HEALTH}')"
  exit 1
fi

# 8e. web-quota-worker 容器健康检查（Playwright Sidecar）
if [ "$WQ_HEALTH" = "healthy" ]; then
  echo "断言通过: web-quota-worker 容器状态 healthy"
else
  echo "断言失败: web-quota-worker 容器健康状态非 healthy (实际: '${WQ_HEALTH}')"
  exit 1
fi

# 9. 全部通过
echo "部署成功: REF=${REF} TAG=${TAG} 镜像 sub2api:${TAG}-w"
