#!/usr/bin/env bash
# disk-guard-maintenance.sh — sub2api 磁盘守卫维护窗口
# 方案 SSOT: docs/disk-guard-plan-20260926.md（整改项②）
#
# 职责（顺序执行）:
#   1. /etc/docker/daemon.json 注入 log-driver=json-file + log-opts(max-size=50m,max-file=5)（合并式，不覆盖其他键）
#   2. journald drop-in SystemMaxUse=500M + 重启 journald
#   3. systemctl restart docker（使 daemon.json 生效）
#   4. 枚举全部 compose 项目并 up -d --force-recreate（使容器 log-opts 生效）
#   5. 换镜像协议: /opt/sub2api/.NEXT_IMAGE_TAG 存在时换新镜像 + 健康检查 90s，失败自动回滚
#   6. 收尾验证: log-opts / 容器健康(120s) / /health / df / journalctl --disk-usage
#
# 幂等: 配置合并式写入（无变化不重写），容器重建天然幂等；重复执行安全。
# DRY_RUN=1 时只打印将执行的动作，不做任何变更（用于人工预检）。
#
# 注意: 本脚本会重启 docker 与全部容器（约 1 分钟窗口），只允许由定时器在维护窗口执行，
#       禁止在业务高峰/AI 会话存活期手动运行（DRY_RUN 除外）。
set -euo pipefail

DRY_RUN="${DRY_RUN:-0}"
ENV_FILE="/opt/sub2api/.env"
MARKER="/opt/sub2api/.NEXT_IMAGE_TAG"
APP_HEALTH_URL="http://127.0.0.1:3300/health"
DAEMON_JSON="/etc/docker/daemon.json"
JOURNALD_DROPIN_DIR="/etc/systemd/journald.conf.d"
JOURNALD_DROPIN="$JOURNALD_DROPIN_DIR/diskguard.conf"
LOG_DIR="/opt/sub2api/scripts/logs"

TS="$(date '+%Y%m%d-%H%M%S')"
LOG_FILE="$LOG_DIR/disk-guard-maintenance-$TS.log"

log() { echo "[$(date '+%F %T')] $*"; }

# -- 日志落文件（含 dry-run 也留档） -----------------------------------------
mkdir -p "$LOG_DIR"
exec > >(tee -a "$LOG_FILE") 2>&1
log "=== disk-guard maintenance start (DRY_RUN=$DRY_RUN) ==="

# -- 命令包装: DRY_RUN 时只打印 ----------------------------------------------
run() {
  if [[ "$DRY_RUN" == "1" ]]; then
    log "[dry-run] $*"
  else
    log "+ $*"
    eval "$@"
  fi
}

# =========================================================================
# 步骤 1: daemon.json 注入 log-driver/log-opts（合并式）
# =========================================================================
log "--- step 1: daemon.json log-opts ---"
if [[ "$DRY_RUN" == "1" ]]; then
  log "[dry-run] python3 merge into $DAEMON_JSON: log-driver=json-file, log-opts={max-size:50m,max-file:5}（保留既有其他键；存在则先备份 .bak-$TS）"
else
  python3 - "$DAEMON_JSON" "$TS" <<'PYEOF'
import json, os, shutil, sys
path, ts = sys.argv[1], sys.argv[2]
target = {"log-driver": "json-file", "log-opts": {"max-size": "50m", "max-file": "5"}}
cfg = {}
existed = os.path.exists(path)
if existed:
    try:
        with open(path) as f:
            cfg = json.load(f)
    except Exception as e:
        print(f"WARN: 既有 daemon.json 解析失败({e})，将按空配置重建")
        cfg = {}
orig = json.dumps(cfg, sort_keys=True, indent=2)
cfg["log-driver"] = target["log-driver"]
cfg["log-opts"] = target["log-opts"]
new = json.dumps(cfg, sort_keys=True, indent=2)
if orig == new:
    print("daemon.json log-opts 已正确，无需改动（幂等跳过）")
else:
    if existed:
        bak = f"{path}.bak-{ts}"
        shutil.copy2(path, bak)
        print(f"已备份原文件 -> {bak}")
    with open(path, "w") as f:
        f.write(new + "\n")
    print("daemon.json 已写入 log-driver/log-opts（合并式，其他键保留）")
PYEOF
fi

# =========================================================================
# 步骤 2: journald SystemMaxUse=500M
# =========================================================================
log "--- step 2: journald drop-in ---"
if [[ ! -f "$JOURNALD_DROPIN" ]] || ! grep -q '^SystemMaxUse=500M$' "$JOURNALD_DROPIN"; then
  run mkdir -p "$JOURNALD_DROPIN_DIR"
  if [[ "$DRY_RUN" == "1" ]]; then
    log "[dry-run] write $JOURNALD_DROPIN: [Journal] SystemMaxUse=500M"
  else
    printf '[Journal]\nSystemMaxUse=500M\n' > "$JOURNALD_DROPIN"
    log "$JOURNALD_DROPIN 已写入"
  fi
else
  log "$JOURNALD_DROPIN 已存在且正确（幂等跳过）"
fi
run systemctl restart systemd-journald

# =========================================================================
# 步骤 3: docker restart（使 daemon.json 生效）
# =========================================================================
log "--- step 3: docker restart ---"
run systemctl restart docker
if [[ "$DRY_RUN" != "1" ]]; then
  for i in $(seq 1 30); do
    systemctl is-active --quiet docker && break
    sleep 1
  done
  systemctl is-active --quiet docker || { log "FATAL: docker restart 后未恢复 active"; exit 1; }
  log "docker 已恢复 active"
fi

# =========================================================================
# 步骤 4: 枚举全部 compose 项目并 force-recreate（使 log-opts 生效）
#   生产现状: 唯一项目 deploy-config（/opt/sub2api/deploy-config/compose.yml，
#   env=/opt/sub2api/.env，7 个服务含 sub2api/postgres/redis/minio/xianyu-worker 组）
# =========================================================================
log "--- step 4: compose 容器重建 ---"
mapfile -t ROWS < <(docker ps --format '{{.Label "com.docker.compose.project"}}\t{{.Label "com.docker.compose.project.working_dir"}}\t{{.Label "com.docker.compose.project.config_files"}}\t{{.Label "com.docker.compose.project.environment_file"}}' | sort -u)
if [[ "${#ROWS[@]}" -eq 0 ]]; then
  log "WARN: 未发现 compose 管理的容器，跳过重建"
fi
for row in "${ROWS[@]}"; do
  IFS=$'\t' read -r proj workdir cfgfiles envfile <<< "$row"
  [[ -n "$proj" && -n "$workdir" && -n "$cfgfiles" ]] || { log "WARN: 标签不完整，跳过: $row"; continue; }
  if [[ -n "$envfile" && -f "$envfile" ]]; then
    envopt="--env-file $envfile"
  else
    log "WARN: 项目 $proj 无 environment_file 标签，尝试不带 --env-file（可能缺插值）"
    envopt=""
  fi
  run "cd '$workdir' && docker compose $envopt -f '$cfgfiles' -p '$proj' up -d --force-recreate"
done

# =========================================================================
# 步骤 5: 换镜像协议（方案 §4）
# =========================================================================
log "--- step 5: image swap protocol ---"
if [[ -s "$MARKER" ]]; then
  TAG="$(tr -d '[:space:]' < "$MARKER")"
  log "发现标记文件 $MARKER -> tag=$TAG"
  if docker image inspect "sub2api:$TAG" >/dev/null 2>&1; then
    if [[ "$DRY_RUN" == "1" ]]; then
      log "[dry-run] cp $ENV_FILE $ENV_FILE.bak-$TS"
      log "[dry-run] sed SUB2API_IMAGE_TAG=$TAG into $ENV_FILE"
      log "[dry-run] docker compose --env-file $ENV_FILE -f /opt/sub2api/deploy-config/compose.yml -p deploy-config up -d --force-recreate sub2api"
      log "[dry-run] health check $APP_HEALTH_URL 最长 90s，失败则回滚（还原 .env 备份 + 旧 tag 重建 + 删标记）"
    else
      ENVBAK="$ENV_FILE.bak-$TS"
      cp "$ENV_FILE" "$ENVBAK"
      log ".env 已备份 -> $ENVBAK"
      sed -i "s/^SUB2API_IMAGE_TAG=.*/SUB2API_IMAGE_TAG=$TAG/" "$ENV_FILE"
      (cd /opt/sub2api/deploy-config && docker compose --env-file "$ENV_FILE" -f compose.yml -p deploy-config up -d --force-recreate sub2api)
      ok=0
      for i in $(seq 1 30); do
        if curl -sf "$APP_HEALTH_URL" >/dev/null 2>&1; then ok=1; break; fi
        sleep 3
      done
      if [[ "$ok" == "1" ]]; then
        log "新镜像 $TAG 上线成功（/health 通过），删除标记文件"
        rm -f "$MARKER"
      else
        log "ROLLBACK: 新镜像 $TAG 90s 健康检查未通过，还原 .env 并用旧镜像重建"
        cp "$ENVBAK" "$ENV_FILE"
        (cd /opt/sub2api/deploy-config && docker compose --env-file "$ENV_FILE" -f compose.yml -p deploy-config up -d --force-recreate sub2api)
        rm -f "$MARKER"
        for i in $(seq 1 20); do
          curl -sf "$APP_HEALTH_URL" >/dev/null 2>&1 && break
          sleep 3
        done
        curl -sf "$APP_HEALTH_URL" >/dev/null 2>&1 && log "回滚完成，/health 恢复" || log "WARN: 回滚后 /health 仍未通过，需人工介入"
      fi
    fi
  else
    log "WARN: 标记存在但镜像 sub2api:$TAG 不存在，跳过换镜像（仅按当前 tag 重建）"
  fi
else
  log "标记文件不存在，跳过换镜像（仅按当前 tag 重建）"
fi

# =========================================================================
# 步骤 6: 收尾验证
# =========================================================================
log "--- step 6: final verification ---"
if [[ "$DRY_RUN" == "1" ]]; then
  log "[dry-run] docker info / 容器 LogConfig / 健康等待 120s / curl /health / df -h / / journalctl --disk-usage"
else
  log "== docker info 默认 log-opts =="
  docker info 2>/dev/null | grep -iE "log" || log "(docker info 无 log 相关行)"
  log "== 各容器 LogConfig（重建后应含 max-file=5 max-size=50m）=="
  for c in $(docker ps --format '{{.Names}}'); do
    docker inspect --format '{{.Name}}  {{.HostConfig.LogConfig}}' "$c"
  done
  log "== 等待容器 healthy（最长 120s）=="
  hokay=0
  for i in $(seq 1 24); do
    bad=0
    while IFS= read -r line; do
      st="${line##* }"
      [[ "$st" == "healthy" || "$st" == "no-hc" ]] || { bad=$((bad+1)); log "  waiting: $line"; }
    done < <(docker ps --format '{{.Names}} {{if .State.Health}}{{.State.Health.Status}}{{else}}no-hc{{end}}')
    if [[ "$bad" -eq 0 ]]; then hokay=1; break; fi
    sleep 5
  done
  [[ "$hokay" == "1" ]] && log "全部容器 healthy（无 healthcheck 的容器视为通过）" || log "WARN: 120s 内仍有容器未 healthy"
  log "== /health =="
  curl -s "$APP_HEALTH_URL" && echo
  log "== df -h / =="
  df -h /
  log "== journalctl --disk-usage =="
  journalctl --disk-usage
fi

log "=== disk-guard maintenance end ==="
exit 0
