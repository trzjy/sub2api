#!/usr/bin/env bash
# =============================================================================
# sub2api 部署前预检守卫（SSOT 唯一合法入口 = deploy-config/scripts/deploy-to-server.sh）
#
# 用法：
#   ./deploy.sh --check         # 仅执行部署前断言（演练 / 巡检），不做任何变更
#   ./deploy.sh [ref]           # 预检通过后，委派 SSOT 入口执行部署（ref 缺省 origin/main）
#
# 设计原则（2026-10-10 compose/端口语义归位后固化）：
#   1. 端口语义 = compose 语义（SSOT：sub2api-ops.md §3.1）：
#        .env SERVER_PORT = 宿主机端口；容器内端口由 compose environment 固定为 8080。
#      → 断言 .env SERVER_PORT == nginx upstream == compose published 端口，且 target == 8080。
#   2. 容器 env 全量来自 .env（compose env_file），不得凭记忆补参数。
#   3. 本脚本只做「部署前拦截」，**不再自己 docker run**。
#      历史 docker run 路径（需要 .env SERVER_PORT=8080 才能监听 8080）与 compose 语义互斥，已废弃 ——
#      曾因两套语义混用导致 2026-10-10 全站 502。
# =============================================================================
set -uo pipefail

APP_DIR="/opt/sub2api"
COMPOSE_FILE="deploy-config/compose.yml"
ENV_FILE="/opt/sub2api/.env"
NGINX_SITE="/etc/nginx/sites-available/duizhang"
STATE_DIR="/root/deploy-state"
ENV_BASELINE="${STATE_DIR}/env-keys-baseline.txt"
DEPLOY_SSOT="${APP_DIR}/deploy-config/scripts/deploy-to-server.sh"

log()  { echo "[$(date '+%F %T')] $*"; }
fail() { echo "[$(date '+%F %T')] ERROR: $*" >&2; exit 1; }

mkdir -p "${STATE_DIR}"

# 从 nginx 站点解析 upstream 端口（捕获组取冒号后端口，直接 grep -oE '[0-9]+' 会先命中 127.0.0.1 的 127）
detect_nginx_port() {
  [ -f "${NGINX_SITE}" ] || return 1
  sed -nE 's|.*proxy_pass[[:space:]]+http://127\.0\.0\.1:([0-9]+)[[:space:]]*;.*|\1|p' \
    "${NGINX_SITE}" 2>/dev/null | head -1
}

preflight() {
  log "preflight: 部署前断言（compose 语义）"
  [ -f "${ENV_FILE}" ] || fail "缺少 ${ENV_FILE}"

  # 断言1：.env SERVER_PORT 存在且为数字（= 宿主机端口）
  local sp
  sp=$(awk -F= '/^SERVER_PORT=/ {print $2}' "${ENV_FILE}" | tail -1 | tr -dc '0-9')
  [ -n "${sp}" ] || fail "断言1 失败: .env SERVER_PORT 缺失/非法"
  log "  断言1 OK  .env SERVER_PORT(宿主端口)=${sp}"

  # 断言2：nginx upstream 必须等于 .env SERVER_PORT
  local up
  up=$(detect_nginx_port || true)
  [ -n "${up}" ] || fail "断言2 失败: 无法从 ${NGINX_SITE} 解析 upstream 端口"
  [ "${up}" = "${sp}" ] || fail "断言2 失败: nginx upstream(${up}) != .env SERVER_PORT(${sp})"
  log "  断言2 OK  nginx upstream=${up} == .env SERVER_PORT"

  # 断言3：.env 必须包含基线中的全部变量（防 env 静默丢失）
  if [ -f "${ENV_BASELINE}" ]; then
    local miss="" total=0 k
    while read -r k; do
      [ -z "${k}" ] && continue
      total=$((total + 1))
      grep -qE "^${k}=" "${ENV_FILE}" || miss="${miss} ${k}"
    done < "${ENV_BASELINE}"
    [ -z "${miss}" ] || fail "断言3 失败: .env 缺失基线变量:${miss}"
    log "  断言3 OK  env 基线比对通过（${total} 项）"
  else
    log "  断言3 跳过: 无基线文件 ${ENV_BASELINE}（执行 audit-env.sh --init 生成）"
  fi

  # 断言4：compose 解析结果必须 published=${sp} 且 target=8080
  local resolved
  resolved=$(cd "${APP_DIR}" && docker compose -f "${COMPOSE_FILE}" \
               --env-file "${ENV_FILE}" config 2>/dev/null)
  [ -n "${resolved}" ] || fail "断言4 失败: docker compose config 无输出（compose 文件/.env 不可用）"
  echo "${resolved}" | grep -q "published: \"${sp}\"" \
    || fail "断言4 失败: compose 解析 published != ${sp}（检查 compose 端口行与 .env SERVER_PORT）"
  echo "${resolved}" | grep -q "target: 8080" \
    || fail "断言4 失败: compose 解析 target != 8080（容器端口应固定 8080）"
  log "  断言4 OK  compose 解析 published=${sp} -> target=8080"

  log "preflight: 全部通过"
}

main() {
  preflight

  if [ "${1:-}" = "--check" ]; then
    log "--check 模式：仅执行部署前断言，未做任何变更"
    exit 0
  fi

  local ref="${1:-origin/main}"
  [ -x "${DEPLOY_SSOT}" ] || fail "SSOT 部署入口不可执行: ${DEPLOY_SSOT}"
  log "预检通过，委派 SSOT 入口执行部署: ${DEPLOY_SSOT} ${ref}"
  exec "${DEPLOY_SSOT}" "${ref}"
}

main "$@"
