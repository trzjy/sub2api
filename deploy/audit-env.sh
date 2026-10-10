#!/usr/bin/env bash
# =============================================================================
# sub2api 环境一致性巡检（audit）
#
# 用法：
#   ./audit-env.sh           # 巡检：env 漂移 / 端口一致性 / nginx upstream
#   ./audit-env.sh --init    # 以当前运行容器的 env 生成基线
#
# 建议：每周执行一次，或接入 cron。有差异时退出码为 1。
#
# 背景：
#   - 2026-10-10 上午：因「.env 34 项 vs 运行容器 47 项」静默漂移 13 项导致 P0 事故，本脚本持续暴露该类漂移。
#   - 2026-10-10 15:45：端口语义归位 —— **compose 语义**：
#       .env SERVER_PORT = 宿主机端口；容器内端口固定 8080（compose environment）。
#     故本脚本断言：.env SERVER_PORT == nginx upstream == 容器宿主端口，且容器端固定 8080 / 容器内 SERVER_PORT=8080。
#     （历史 docker run 语义要求 .env SERVER_PORT=8080，与 compose 互斥，已废弃。）
# =============================================================================
set -uo pipefail

CONTAINER="sub2api"
ENV_FILE="/opt/sub2api/.env"
NGINX_SITE="/etc/nginx/sites-available/duizhang"
STATE_DIR="/root/deploy-state"
BASELINE="${STATE_DIR}/env-keys-baseline.txt"

mkdir -p "${STATE_DIR}"

log()  { echo "[$(date '+%F %T')] $*"; }
warn() { echo "[$(date '+%F %T')] WARN: $*" >&2; }
err()  { echo "[$(date '+%F %T')] ERROR: $*" >&2; }

PROBLEM=0

# -----------------------------------------------------------------------------
# --init：以运行容器 env 生成基线
# -----------------------------------------------------------------------------
do_init() {
  docker ps -a --format '{{.Names}}' | grep -qx "${CONTAINER}" \
    || { err "容器 ${CONTAINER} 不存在，无法生成基线"; exit 1; }

  # 排除 PATH：镜像自带 ENV，不由 .env 提供，计入基线会恒定误报
  docker inspect "${CONTAINER}" --format '{{range .Config.Env}}{{println .}}{{end}}' \
    | awk -F= 'NF>0 {print $1}' | grep -v '^PATH$' | sort -u > "${BASELINE}"

  log "基线已生成: ${BASELINE}（$(grep -c . "${BASELINE}") 项）"
  log "来源: 运行容器 ${CONTAINER} 的实际 env（= 已验证可运行的期望状态）"
  exit 0
}

[ "${1:-}" = "--init" ] && do_init

# -----------------------------------------------------------------------------
# 巡检
# -----------------------------------------------------------------------------
log "=== sub2api 环境一致性巡检 ==="

[ -f "${ENV_FILE}" ] || { err ".env 不存在: ${ENV_FILE}"; exit 1; }

env_keys=$(awk -F= '/^[[:space:]]*[^#]/ && NF>0 {print $1}' "${ENV_FILE}" | sed 's/[[:space:]]*$//' | sort -u)

if docker ps -a --format '{{.Names}}' | grep -qx "${CONTAINER}"; then
  run_env_raw=$(docker inspect "${CONTAINER}" --format '{{range .Config.Env}}{{println .}}{{end}}')
  run_keys=$(echo "${run_env_raw}" | awk -F= 'NF>0 {print $1}' | sort -u)
else
  run_keys=""
  run_env_raw=""
  warn "容器 ${CONTAINER} 不存在，跳过运行态比对"
fi

echo
echo "--- [1] env 基线比对 ---"
if [ -f "${BASELINE}" ]; then
  total=$(grep -c . "${BASELINE}")
  echo "基线项数: ${total}  |  .env 项数: $(echo "${env_keys}" | grep -c .)  |  运行容器: $(echo "${run_keys}" | grep -c .)"

  miss_env=$(comm -23 "${BASELINE}" <(echo "${env_keys}"))
  if [ -n "${miss_env}" ]; then
    err ".env 缺失基线变量:"; echo "${miss_env}" | sed 's/^/    - /'
    PROBLEM=1
  else
    echo "OK  .env 包含基线全部变量"
  fi

  if [ -n "${run_keys}" ]; then
    miss_run=$(comm -23 "${BASELINE}" <(echo "${run_keys}"))
    if [ -n "${miss_run}" ]; then
      err "运行容器缺失基线变量:"; echo "${miss_run}" | sed 's/^/    - /'
      PROBLEM=1
    else
      echo "OK  运行容器包含基线全部变量"
    fi
  fi
else
  warn "无基线文件 ${BASELINE}，执行 $0 --init 生成"
fi

echo
echo "--- [2] 端口一致性（compose 语义：.env SERVER_PORT=宿主端口；容器内固定 8080） ---"
sp=$(awk -F= '/^SERVER_PORT=/ {print $2}' "${ENV_FILE}" | tail -1 | tr -dc '0-9')
echo ".env SERVER_PORT(宿主端口) = ${sp:-<未设置>}"
[ -n "${sp}" ] || { err "SERVER_PORT 未设置"; PROBLEM=1; }

binding=""; cport=""; hip=""; hport=""; cport_env=""
if [ -n "${run_keys}" ]; then
  binding=$(docker inspect "${CONTAINER}" --format \
    '{{range $c,$v := .HostConfig.PortBindings}}{{$c}}|{{(index $v 0).HostIp}}:{{(index $v 0).HostPort}}{{end}}')
  cport="${binding%%/*}"                 # 容器端口（/tcp 前）
  hip_hport="${binding#*|}"              # 127.0.0.1:3300
  hip="${hip_hport%:*}"                  # 127.0.0.1
  hport="${hip_hport##*:}"               # 3300
  cport_env=$(echo "${run_env_raw}" | awk -F= '$1=="SERVER_PORT"{print $2}')
  echo "容器端口映射   = ${binding}  (容器端=${cport} 宿主端=${hip}:${hport})"
  echo "容器内 SERVER_PORT = ${cport_env:-<未设置>}"

  [ "${cport}" = "8080" ] || { err "容器端应为 8080（compose 固定），实际 ${cport}"; PROBLEM=1; }
  [ "${cport_env}" = "8080" ] || { err "容器内 SERVER_PORT 应为 8080，实际 ${cport_env}"; PROBLEM=1; }
  if [ -n "${sp}" ] && [ "${hport}" != "${sp}" ]; then
    err ".env SERVER_PORT(${sp}) != 容器宿主端口(${hport})"; PROBLEM=1
  fi
  [ "${hip}" = "127.0.0.1" ] || warn "宿主绑定 IP 非 127.0.0.1（实际 ${hip}）"
fi

if [ -f "${NGINX_SITE}" ]; then
  up=$(sed -nE 's|.*proxy_pass[[:space:]]+http://127\.0\.0\.1:([0-9]+)[[:space:]]*;.*|\1|p' "${NGINX_SITE}" | head -1)
  echo "nginx upstream = ${up:-<未解析>}"
  if [ -n "${up}" ] && [ -n "${sp}" ] && [ "${up}" != "${sp}" ]; then
    err "nginx upstream(${up}) != .env SERVER_PORT(${sp})"; PROBLEM=1
  elif [ -n "${up}" ] && [ -n "${hport}" ] && [ "${up}" != "${hport}" ]; then
    err "nginx upstream(${up}) != 容器宿主端口(${hport})"; PROBLEM=1
  elif [ -n "${up}" ]; then
    echo "OK  nginx upstream == 宿主端口 == .env SERVER_PORT"
  fi
else
  warn "nginx 站点文件不存在: ${NGINX_SITE}"
fi

echo
echo "--- [3] 运行容器 env 与 .env 的值差异（敏感项仅提示；SERVER_PORT 例外见下） ---"
if [ -n "${run_keys}" ]; then
  printf '%s\n' "${run_env_raw}" | sort > /tmp/.run_env
  awk -F= '/^[[:space:]]*[^#]/ && NF>0 {print}' "${ENV_FILE}" | sort > /tmp/.file_env
  seen_any=0
  while IFS= read -r line; do
    k="${line%%=*}"
    # SERVER_PORT 例外：.env=宿主端口(3300)，容器内=8080，属 compose 设计内差异，跳过
    [ "${k}" = "SERVER_PORT" ] && continue
    fv=$(awk -F= -v key="$k" '$1==key {sub(/^[^=]*=/,""); print}' /tmp/.file_env)
    if [ -n "${fv}" ] && [ "${line#*=}" != "${fv}" ]; then
      seen_any=1
      case "${k}" in
        *KEY*|*SECRET*|*PASS*|*TOKEN*|*DSN*|*CREDENTIAL*)
          echo "  DIFF(敏感) ${k}: 值不同，长度 run=${#line} file=${#fv}" ;;
        *)
          echo "  DIFF ${k}: run=${line#*=}  file=${fv}" ;;
      esac
    fi
  done < /tmp/.run_env
  rm -f /tmp/.run_env /tmp/.file_env
  [ "${seen_any}" -eq 0 ] && echo "OK  无非预期值差异（SERVER_PORT 按 compose 设计已排除）"
fi

echo
if [ "${PROBLEM}" -eq 0 ]; then
  log "巡检结论: PASS（未发现漂移）"
else
  err "巡检结论: FAIL（存在漂移，请修复后再部署）"
fi
exit "${PROBLEM}"
