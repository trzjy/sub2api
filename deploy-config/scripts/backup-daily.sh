#!/usr/bin/env bash
set -euo pipefail

# backup-daily.sh —— sub2api 每日本地备份（方案 SSOT：docs/backup-daily-plan.md）
#
# 四类备份：
#   1. postgres  : pg_dump -Fc（自定义格式自带压缩）
#   2. xianyu MySQL: mysqldump --single-transaction | gzip
#   3. minio     : 数据目录文件级增量同步（rsync 镜像，不追加）
#   4. data/ 配置子集: tar（排除 logs/ 与 model_pricing.json）
#
# 增长控制：恒定 14 桶 = 日备 ×7 + 周备 ×4（周日份）+ 月备 ×3（1 号份），滚动删除超龄桶。
# 硬闸（失败关闭，绝不以旧备份冒充成功）：备份目录 >15GB、当日 pg dump 环比 +50%、
# pg_restore --list 或 gzip -t 校验失败 —— 任一命中即 stderr 报错 + 非零退出。
#
# 凭据一律取自容器自身环境变量（docker exec 内展开），宿主机与本脚本均不出现明文。

# ── 可调参数（env 覆盖，便于滚动策略调小实证） ────────────────────────────────
BACKUP_ROOT="${BACKUP_ROOT:-/opt/sub2api-backup}"
DATA_DIR="${DATA_DIR:-/opt/sub2api-data}"
PG_CONTAINER="${PG_CONTAINER:-sub2api-postgres}"
MYSQL_CONTAINER="${MYSQL_CONTAINER:-xianyu-worker-mysql}"
MYSQL_DUMP_DB="${MYSQL_DUMP_DB:-xianyu}"
KEEP_DAILY="${KEEP_DAILY:-7}"
KEEP_WEEKLY="${KEEP_WEEKLY:-4}"
KEEP_MONTHLY="${KEEP_MONTHLY:-3}"
SIZE_LIMIT_BYTES="${SIZE_LIMIT_BYTES:-16106127360}"   # 15GB
PG_GROWTH_LIMIT_PCT="${PG_GROWTH_LIMIT_PCT:-50}"      # 环比 +50%
LOCK_FILE="${LOCK_FILE:-/var/lock/sub2api-backup.lock}"

TS="$(date +%Y%m%d-%H%M%S)"
DEST="${BACKUP_ROOT}/backup-${TS}"

log()  { printf '[backup %s] %s\n' "${TS}" "$*"; }
warn() { printf '[backup %s][WARN] %s\n' "${TS}" "$*" >&2; }
die()  { printf '[backup %s][FAIL] %s\n' "${TS}" "$*" >&2; exit 1; }

# ── 互斥锁：防 systemd 与人工触发并发 ─────────────────────────────────────────
exec 9>"${LOCK_FILE}"
if ! flock -n 9; then
  die "已有备份进程在运行（锁 ${LOCK_FILE}），本次放弃"
fi

mkdir -p "${BACKUP_ROOT}"
START_TS=$(date +%s)

# ── 1. postgres：pg_dump -Fc ────────────────────────────────────────────────
log "1/4 导出 postgres（容器 ${PG_CONTAINER}）"
PG_FILE="${DEST}/pg.dump"
t0=$(date +%s)
mkdir -p "${DEST}"
# 凭据在容器内由 sh 展开：PGPASSWORD/POSTGRES_USER/POSTGRES_DB 均来自容器环境
if ! docker exec "${PG_CONTAINER}" sh -c \
  'PGPASSWORD="$POSTGRES_PASSWORD" pg_dump -U "$POSTGRES_USER" -Fc "$POSTGRES_DB"' \
  > "${PG_FILE}"; then
  rm -rf "${DEST}"
  die "pg_dump 执行失败，当日备份作废"
fi
PG_SEC=$(( $(date +%s) - t0 ))
PG_SIZE=$(stat -c %s "${PG_FILE}")
[ "${PG_SIZE}" -gt 0 ] || { rm -rf "${DEST}"; die "pg_dump 产出 0 字节，当日备份作废"; }
log "    pg.dump ${PG_SIZE} 字节 / ${PG_SEC}s"

# ── 2. xianyu MySQL：mysqldump --single-transaction | gzip ──────────────────
log "2/4 导出 mysql 库 ${MYSQL_DUMP_DB}（容器 ${MYSQL_CONTAINER}）"
MYSQL_FILE="${DEST}/mysql.sql.gz"
t0=$(date +%s)
# 密码同样在容器内由 sh 展开（-p 参数不进宿主机命令行）
if ! docker exec "${MYSQL_CONTAINER}" sh -c \
  "mysqldump -u root -p\"\$MYSQL_ROOT_PASSWORD\" --single-transaction --databases ${MYSQL_DUMP_DB}" \
  | gzip -9 > "${MYSQL_FILE}"; then
  rm -rf "${DEST}"
  die "mysqldump 执行失败，当日备份作废"
fi
MYSQL_SEC=$(( $(date +%s) - t0 ))
MYSQL_SIZE=$(stat -c %s "${MYSQL_FILE}")
[ "${MYSQL_SIZE}" -gt 0 ] || { rm -rf "${DEST}"; die "mysqldump 产出 0 字节，当日备份作废"; }
log "    mysql.sql.gz ${MYSQL_SIZE} 字节 / ${MYSQL_SEC}s"

# ── 3. minio：数据目录文件级增量同步（镜像当前状态，不追加） ────────────────
log "3/4 同步 minio 数据目录"
MINIO_DIR="${DEST}/minio"
MINIO_SEC=0
mkdir -p "${MINIO_DIR}"
if [ -d "${DATA_DIR}/minio_data" ]; then
  t0=$(date +%s)
  rsync -a --delete "${DATA_DIR}/minio_data/" "${MINIO_DIR}/"
  MINIO_SEC=$(( $(date +%s) - t0 ))
else
  warn "${DATA_DIR}/minio_data 不存在，跳过 minio 同步"
fi
log "    minio/ 同步完成 / ${MINIO_SEC}s"

# ── 4. data/ 配置子集：tar（排除 logs/ 与 model_pricing.json） ─────────────
log "4/4 打包 data/ 配置子集"
DATA_TAR="${DEST}/data-config.tar.gz"
DATA_SEC=0
DATA_ITEMS=()
for item in config.yaml catfk free-watch pages plugins; do
  [ -e "${DATA_DIR}/data/${item}" ] && DATA_ITEMS+=("${item}")
done
# config.yaml 的历史 bak 一并纳入
while IFS= read -r bak; do
  [ -n "${bak}" ] && DATA_ITEMS+=("${bak}")
done < <(cd "${DATA_DIR}/data" && ls -1 config.yaml.bak-* 2>/dev/null || true)
if [ "${#DATA_ITEMS[@]}" -eq 0 ]; then
  warn "data/ 下无任何配置项，产出空包占位"
fi
t0=$(date +%s)
tar -czf "${DATA_TAR}" \
  --exclude='logs' --exclude='logs/*' \
  --exclude='model_pricing.json' --exclude='model_pricing.sha256' \
  -C "${DATA_DIR}/data" "${DATA_ITEMS[@]}"
DATA_SEC=$(( $(date +%s) - t0 ))
DATA_SIZE=$(stat -c %s "${DATA_TAR}")
log "    data-config.tar.gz ${DATA_SIZE} 字节 / ${DATA_SEC}s（条目 ${#DATA_ITEMS[@]} 项）"

# ── 完整性校验（硬闸 ③）：失败即当日备份失败 ─────────────────────────────────
log "校验 pg.dump 可读性（pg_restore --list）"
if ! docker cp "${PG_FILE}" "${PG_CONTAINER}:/tmp/backup-verify-pg.dump" >/dev/null; then
  rm -rf "${DEST}"
  die "pg.dump 传入校验容器失败，当日备份作废"
fi
if ! docker exec "${PG_CONTAINER}" pg_restore --list /tmp/backup-verify-pg.dump >/dev/null; then
  docker exec "${PG_CONTAINER}" rm -f /tmp/backup-verify-pg.dump >/dev/null 2>&1 || true
  rm -rf "${DEST}"
  die "pg_restore --list 校验失败，dump 不可读，当日备份作废"
fi
docker exec "${PG_CONTAINER}" rm -f /tmp/backup-verify-pg.dump >/dev/null 2>&1 || true

log "校验 mysql.sql.gz（gzip -t）"
if ! gzip -t "${MYSQL_FILE}" 2>/dev/null; then
  rm -rf "${DEST}"
  die "gzip -t 校验失败，mysql dump 损坏，当日备份作废"
fi

# ── manifest ────────────────────────────────────────────────────────────────
log "生成 manifest"
MANIFEST="${DEST}/manifest.txt"
{
  echo "backup_ts=${TS}"
  echo "created_at=$(date -Is)"
  echo "backup_root=${BACKUP_ROOT}"
  echo "pg_size_bytes=${PG_SIZE}"
  echo "pg_elapsed_sec=${PG_SEC}"
  echo "mysql_size_bytes=${MYSQL_SIZE}"
  echo "mysql_elapsed_sec=${MYSQL_SEC}"
  echo "minio_size_bytes=$(du -sb "${MINIO_DIR}" | cut -f1)"
  echo "minio_elapsed_sec=${MINIO_SEC}"
  echo "data_size_bytes=${DATA_SIZE}"
  echo "data_elapsed_sec=${DATA_SEC}"
  echo "--- sha256 ---"
  ( cd "${DEST}" && sha256sum pg.dump mysql.sql.gz data-config.tar.gz )
  echo "--- verify ---"
  echo "pg_restore_list=ok"
  echo "gzip_t=ok"
} > "${MANIFEST}"
log "    manifest.txt 就绪"

# ── 滚动保留：恒定 KEEP_DAILY + KEEP_WEEKLY + KEEP_MONTHLY 桶 ────────────────
log "滚动保留（日 ${KEEP_DAILY} / 周 ${KEEP_WEEKLY} / 月 ${KEEP_MONTHLY}）"
declare -a ALL_BUCKETS=()
while IFS= read -r line; do
  ALL_BUCKETS+=("${line}")
done < <(find "${BACKUP_ROOT}" -maxdepth 1 -mindepth 1 -type d -name 'backup-[0-9]*' | sort -r)

declare -A KEEP_SET=()
mark_keep() { KEEP_SET["$1"]=1; }

# a) 最近 KEEP_DAILY 个桶（含本次）
for i in "${!ALL_BUCKETS[@]}"; do
  [ "${i}" -ge "${KEEP_DAILY}" ] && break
  mark_keep "${ALL_BUCKETS[$i]}"
done

# b) 最近 KEEP_WEEKLY 个周日份桶
w=0
for b in "${ALL_BUCKETS[@]}"; do
  d=$(basename "${b}" | cut -d- -f2)
  dow=$(date -d "${d:0:4}-${d:4:2}-${d:6:2}" +%u)
  [ "${dow}" = "7" ] || continue
  mark_keep "${b}"
  w=$((w+1))
  [ "${w}" -ge "${KEEP_WEEKLY}" ] && break
done

# c) 最近 KEEP_MONTHLY 个 1 号份桶
m=0
for b in "${ALL_BUCKETS[@]}"; do
  d=$(basename "${b}" | cut -d- -f2)
  [ "${d:6:2}" = "01" ] || continue
  mark_keep "${b}"
  m=$((m+1))
  [ "${m}" -ge "${KEEP_MONTHLY}" ] && break
done

DELETED=0
for b in "${ALL_BUCKETS[@]}"; do
  [ -n "${KEEP_SET[$b]:-}" ] && continue
  rm -rf "${b}"
  DELETED=$((DELETED+1))
  log "    删除超龄桶: $(basename "${b}")"
done
log "    当前桶数=${#ALL_BUCKETS[@]} 保留=$((${#ALL_BUCKETS[@]}-DELETED)) 删除=${DELETED}"

# ── 硬闸 ②：当日 pg dump 环比膨胀 ───────────────────────────────────────────
log "硬闸：pg dump 环比 +${PG_GROWTH_LIMIT_PCT}% 检查"
PREV_PG=""
for b in "${ALL_BUCKETS[@]}"; do
  [ "${b}" = "${DEST}" ] && continue
  if [ -f "${b}/pg.dump" ]; then PREV_PG="${b}/pg.dump"; break; fi
done
if [ -n "${PREV_PG}" ]; then
  PREV_SIZE=$(stat -c %s "${PREV_PG}")
  LIMIT=$(( PREV_SIZE * (100 + PG_GROWTH_LIMIT_PCT) / 100 ))
  if [ "${PG_SIZE}" -gt "${LIMIT}" ]; then
    die "pg dump 环比异常膨胀：本次 ${PG_SIZE} > 上次 ${PREV_SIZE} 的 ${PG_GROWTH_LIMIT_PCT}% 阈值 ${LIMIT}（基准 $(basename "$(dirname "${PREV_PG}")")）"
  fi
  log "    环比 OK（上次 ${PREV_SIZE} → 本次 ${PG_SIZE}）"
else
  log "    无上次基准可比对，跳过环比闸"
fi

# ── 硬闸 ①：备份目录总量上界 ───────────────────────────────────────────────
log "硬闸：备份目录总量检查"
TOTAL_BYTES=$(du -sb "${BACKUP_ROOT}" | cut -f1)
if [ "${TOTAL_BYTES}" -gt "${SIZE_LIMIT_BYTES}" ]; then
  die "备份目录总量 ${TOTAL_BYTES} 字节超过上界 ${SIZE_LIMIT_BYTES} 字节（15GB）"
fi
log "    总量 ${TOTAL_BYTES} 字节 < 上界 ${SIZE_LIMIT_BYTES} 字节"

TOTAL_SEC=$(( $(date +%s) - START_TS ))
{
  echo "--- summary ---"
  echo "buckets_total=${#ALL_BUCKETS[@]}"
  echo "buckets_deleted=${DELETED}"
  echo "backup_root_total_bytes=${TOTAL_BYTES}"
  echo "total_elapsed_sec=${TOTAL_SEC}"
  echo "result=success"
} >> "${MANIFEST}"

log "备份完成：${DEST}（耗时 ${TOTAL_SEC}s）"
exit 0