#!/usr/bin/env bash
set -euo pipefail

# predeploy-migration-guard.sh —— 部署前「迁移不可变」门禁（服务器端执行）
#
# 背景：2026-10-01 提交 657cd2d16 自述「注释级、零语义」，实际改了 4 个已在生产库
# 应用的迁移文件（257/263/264/265）。启动时 schema_migrations 的 checksum 校验失败
# → sub2api fail-fast 退出 → 容器重启循环 → 源站无响应 → Cloudflare 502 全站中断。
# 本门禁把「误改已应用迁移」拦在 build 之前。
#
# 语义（见 backend/migrations/README.md §Immutability Principle）：
#   迁移一旦应用到任何环境（含生产），就不得再修改；启动校验的是「文件名 + 全文
#   checksum」，哪怕只改注释一行的字节也会 mismatch。
#
# 判定规则：
#   A（新增）                      → 放行
#   M/D（修改/删除）且库内已应用    → 硬阻断 exit 1
#   M/D 但库内未应用               → 警告放行
#   基线 sha 无法解析/缺失          → 警告放行（输出明确说明未做迁移差异检查）
#   postgres 查询失败              → fail-closed 阻断 exit 1（无法确认是否已应用）
#   基线 == 目标                   → 放行
#
# 破例开关：ALLOW_APPLIED_MIGRATION_CHANGE=1 时，已应用迁移的改动降级为警告放行。
#   仅在人类已明确确认「要改库内 checksum」的场景使用。
#
# 禁区：脚本内不得出现删除类命令或同步删除工具，亦不得含任何删除逻辑。

# 1. 用法：predeploy-migration-guard.sh [TARGET_REF]，缺省 origin/main
REF="${1:-origin/main}"
ALLOW_OVERRIDE="${ALLOW_APPLIED_MIGRATION_CHANGE:-0}"

# 2. 固定服务器工作目录
cd /opt/sub2api

ENV_FILE="/opt/sub2api/.env"
MIG_PATH="backend/migrations"
PG_CONTAINER="sub2api-postgres"

echo "=== 部署前迁移不可变门禁 ==="
echo "目标 REF      : ${REF}"
echo "部署工作目录  : /opt/sub2api"
echo "破例开关      : ALLOW_APPLIED_MIGRATION_CHANGE=${ALLOW_OVERRIDE}"
echo

# 3. 解析目标 commit sha
#    --verify 保证必须是 commit-ish；缺省 .git 或 ref 不存在时按 fail-closed 处理。
if TARGET_SHA=$(git rev-parse --verify --short "${REF}^{commit}" 2>/dev/null); then
  echo "目标 sha      : ${TARGET_SHA}"
else
  echo "警告: 目标 REF '${REF}' 在当前仓库不可解析，先尝试 git fetch origin 后重试"
  git fetch origin >/dev/null 2>&1 || { echo "提示: git fetch origin 失败（可能无外网），继续沿用本地对象库"; }
  if TARGET_SHA=$(git rev-parse --verify --short "${REF}^{commit}" 2>/dev/null); then
    echo "目标 sha      : ${TARGET_SHA}"
  else
    echo "错误: 目标 REF '${REF}' 仍不可解析，无法确定要部署的内容，保守阻断"
    exit 1
  fi
fi

# 4. 基线 sha = 当前正在运行的镜像 tag（形如 fdaeded35-w，去尾部 -w 得 short sha）
BASE_RAW=""
if [ -r "$ENV_FILE" ]; then
  BASE_RAW=$(grep -E '^SUB2API_IMAGE_TAG=' "$ENV_FILE" | tail -1 | cut -d= -f2- | tr -d '\r')
fi

BASE_SHA=""
if [ -z "$BASE_RAW" ]; then
  echo "警告: ${ENV_FILE} 中未取到 SUB2API_IMAGE_TAG，基线未知"
else
  BASE_SHA="${BASE_RAW%-w}"
  echo "运行镜像 tag  : ${BASE_RAW}  →  基线 sha 候选 ${BASE_SHA}"
fi

if [ -z "$BASE_SHA" ] || ! git cat-file -e "${BASE_SHA}^{commit}" 2>/dev/null; then
  echo
  echo "警告: 基线 sha '${BASE_SHA}' 在本仓库不可解析（运行的是仓库里没有的旧镜像？）"
  echo "      → 本次**未做迁移差异检查**（backend/migrations 未被校验），直接放行以不卡死部署"
  echo "      → 若目标版本确实含迁移改动，请人工核对 backend/migrations/README.md 不可变原则"
  echo "放行: 基线未知，跳过迁移差异检查（TARGET=${TARGET_SHA}）"
  exit 0
fi

# 5. 基线 == 目标 → 无差异，直接放行
if [ "$BASE_SHA" = "$TARGET_SHA" ]; then
  echo "基线 sha      : ${BASE_SHA}（与运行镜像一致）"
  echo "判定: 基线 == 目标，backend/migrations 无差异"
  echo "放行: 无迁移文件改动（TARGET=${TARGET_SHA}）"
  exit 0
fi

# 6. 取迁移目录差异
#    --no-renames：把重命名拆成 D + A，便于按「修改/删除」与「新增」分别判定。
DIFF_OUT=$(git diff --name-status --no-renames "${BASE_SHA}" "${TARGET_SHA}" -- "${MIG_PATH}/" || true)

if [ -z "$DIFF_OUT" ]; then
  echo "判定: ${BASE_SHA}..${TARGET_SHA} 之间 ${MIG_PATH}/ 无任何改动"
  echo "放行: 无迁移文件改动（TARGET=${TARGET_SHA}）"
  exit 0
fi

echo "迁移差异清单（${BASE_SHA}..${TARGET_SHA}）:"
echo "$DIFF_OUT" | sed 's/^/  /'
echo

# 7. 逐个文件分类判定
PG_FAIL=0
BLOCKED_FILES=()
CHANGED_FILES=()

while read -r STATUS FILEPATH; do
  [ -n "${STATUS:-}" ] || continue
  [ -n "${FILEPATH:-}" ] || continue
  FNAME="${FILEPATH##*/}"

  case "$STATUS" in
    A)
      echo "放行(A 新增): ${FNAME} —— 新增迁移文件不影响已应用记录的 checksum"
      continue
      ;;
  esac

  # M/D（及其他非新增状态，如 T 类型变更）一律按「改动已存在文件」处理
  CHANGED_FILES+=("${STATUS} ${FNAME}")

  # 查库：filename 用 psql 变量绑定（-v fname=... + :'fname'）传参，避免字符串拼接注入。
  # 注意：psql 在 -c 模式下不做变量插值（官方限制），故走 stdin + -f - 的正常处理模式；
  # -v ON_ERROR_STOP=1 使连接/权限/对象缺失等错误体现为非 0 退出码。
  RESULT=""
  if ! RESULT=$(printf '%s' "SELECT 1 FROM schema_migrations WHERE filename = :'fname' LIMIT 1" \
        | docker exec -i "$PG_CONTAINER" psql -U sub2api -d sub2api \
            -v ON_ERROR_STOP=1 -v fname="$FNAME" -tA -f - 2>/dev/null); then
    PG_FAIL=1
    echo "错误: 查询 schema_migrations 失败（${FNAME}）：postgres 不可连或查询异常"
    continue
  fi

  if [ "$(printf '%s' "$RESULT" | tr -d '[:space:]')" = "1" ]; then
    echo "命中: ${FNAME} 已在生产库 schema_migrations 中应用（状态 ${STATUS}）"
    BLOCKED_FILES+=("${STATUS} ${FNAME}")
  else
    echo "安全: ${FNAME} 未在库应用（状态 ${STATUS}，查询为空）→ 改动不影响启动 checksum 校验"
  fi
done <<< "$DIFF_OUT"

# 8. postgres 不可用 → fail-closed 阻断
if [ "$PG_FAIL" -ne 0 ]; then
  echo
  echo "错误: 无法确认上述迁移文件是否已应用，保守阻断（fail-closed）"
  echo "      postgres 都不可用时，本次部署本身也不应继续"
  echo "阻断(exit 1): 数据库不可用，迁移 gates 无法完成"
  exit 1
fi

# 9. 汇总
echo
if [ "${#CHANGED_FILES[@]}" -gt 0 ]; then
  echo "非新增类改动文件（已逐个查库）:"
  for line in "${CHANGED_FILES[@]}"; do
    echo "  ${line}"
  done
fi

if [ "${#BLOCKED_FILES[@]}" -eq 0 ]; then
  echo "放行(exit 0): 无「已应用迁移被改动」的情况（TARGET=${TARGET_SHA}）"
  exit 0
fi

echo "===================================================================="
echo "阻断 · 以下迁移文件已应用于生产库，却在 ${BASE_SHA}..${TARGET_SHA} 中被改动:"
echo "===================================================================="
for line in "${BLOCKED_FILES[@]}"; do
  echo "  [BLOCK] ${line}"
done
echo
echo "为什么阻断:"
echo "  sub2api 启动时会用「文件名 + 全文 checksum」比对 schema_migrations 记录。"
echo "  改动哪怕只有注释一行的字节，也会 checksum mismatch → 应用 fail-fast 退出"
echo "  → 容器 Restarting 重启循环 → 源站无响应 → Cloudflare 502 全站中断。"
echo "  （2026-10-01 事故：提交 657cd2d16 改了已应用的 257/263/264/265，全站中断 10 分钟）"
echo
echo "处置建议（任选其一，不要用 --force 蒙混）:"
echo "  1) 还原：git checkout ${BASE_SHA} -- backend/migrations/<文件>，把文件恢复到被应用时的内容"
echo "  2) 新建：保留原文件不动，另起一个新序号的迁移文件承载本次变更"
echo "  3) 确有改库 checksum 的必要时：人工确认后再以 ALLOW_APPLIED_MIGRATION_CHANGE=1 重跑"

if [ "$ALLOW_OVERRIDE" = "1" ]; then
  echo
  echo "####################################################################"
  echo "## 醒目警告: ALLOW_APPLIED_MIGRATION_CHANGE=1 已生效，破例放行        ##"
  echo "## 已应用迁移被改动，启动必然 checksum mismatch → 重启循环 → 502      ##"
  echo "## 继续前请确认：已同步更新 / 将重建 schema_migrations 的 checksum    ##"
  echo "####################################################################"
  echo "放行(exit 0): 破例开关生效，降级为警告（TARGET=${TARGET_SHA}）"
  exit 0
fi

echo "阻断(exit 1): ${#BLOCKED_FILES[@]} 个已应用迁移被改动，部署中止于构建之前"
exit 1
