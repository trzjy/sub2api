#!/usr/bin/env bash
# C8 复跑（整改后）关键项重验脚本（派发单 D-CURSOR-030）
#
# 用途：在「修复后」代码上复跑 C8 关键项（全新库 sub2api_cursor_030 起 server），
#       逐条核证整改项并把请求/响应与 psql 直查结果原样落盘为证据。
#
# 覆盖（对应派发单 §2 重验清单）：
#   #1 契约  ：GET /admin/quota_keys 分页信封(items/total) + code_masked；sweep 返回 {swept}
#   #2 / ≤15 ：单 key 建号至 15 上限 → 第 16 个被拒
#   #3       ：cursor 账号编辑（不改归属）→ 成功
#   #6       ：PUT expires_at 显式 null → 清空；省略 → 不变
#   #7       ：ops_job_heartbeats 含 cursor_account_retention（跑一轮后）
#
# 本脚本只读业务接口、只写本地验收库（sub2api_cursor_030）的测试夹具，不改任何业务代码。
#
# 用法：
#   BASE_URL=http://127.0.0.1:8098 \
#   ADMIN_JWT=<jwt> \
#   EVIDENCE_DIR=~/.sub2api-acceptance/sub2api-cursor-20261013/08-c8-evidence-r2 \
#   PG_DSN="postgresql://sub2api:sub2api@127.0.0.1:5432/sub2api_cursor_030" \
#   ./backend/scripts/c8-r2-reverify.sh
#
# 退出码：0 = 全部断言通过；1 = 存在未通过断言（证据仍会落盘）。

set -uo pipefail

BASE_URL="${BASE_URL:-http://127.0.0.1:8098}"
ADMIN_JWT="${ADMIN_JWT:-}"
EVIDENCE_DIR="${EVIDENCE_DIR:-$HOME/.sub2api-acceptance/sub2api-cursor-20261013/08-c8-evidence-r2}"
PG_DSN="${PG_DSN:-postgresql://sub2api:sub2api@127.0.0.1:5432/sub2api_cursor_030}"
# #7 等待心跳的最长时间（秒）：cursor_account_retention 启动延迟 5min 后写首轮心跳
HEARTBEAT_MAX_WAIT="${HEARTBEAT_MAX_WAIT:-600}"

CODE_CAP="R2E2E-TEST-CODE-0001"        # 已存在的 key 1（≤15 上限用）
CODE_EXPIRED="R2E2E-EXPIRED-0009"      # sweep 用过期 key
CODE_TRI_STATE="R2E2E-TRISTATE-0008"   # expires_at 三态用 key

mkdir -p "$EVIDENCE_DIR"
PASS_COUNT=0
FAIL_COUNT=0

log() { printf '%s\n' "$*"; }

# assert <描述> <实际是否通过(0/1)> <证据文件>
assert() {
  local desc="$1" ok="$2" ev="${3:-}"
  if [ "$ok" = "0" ]; then
    PASS_COUNT=$((PASS_COUNT + 1))
    log "  ✅ $desc"
  else
    FAIL_COUNT=$((FAIL_COUNT + 1))
    log "  ❌ $desc${ev:+（证据：$ev）}"
  fi
}

# http <步骤号> <名字> <方法> <路径> [json-body]
http() {
  local step="$1" name="$2" method="$3" path="$4" body="${5:-}"
  local req_file="$EVIDENCE_DIR/${step}-${name}-request"
  local resp_file="$EVIDENCE_DIR/${step}-${name}-response.txt"
  local url="${BASE_URL}${path}"
  local args=( -s -m 60 -X "$method" "$url" -H "Authorization: Bearer ${ADMIN_JWT}" )
  if [ -n "$body" ]; then
    printf '%s' "$body" > "${req_file}.json"
    log "  → REQUEST  ${method} ${path}"
    log "    body: ${body}"
    args+=( -H 'Content-Type: application/json' -d "$body" )
  else
    printf '%s %s\n' "$method" "$path" > "${req_file}.txt"
    log "  → REQUEST  ${method} ${path}"
  fi
  args+=( -D "$EVIDENCE_DIR/${step}-${name}-headers.txt" )
  local out status
  out="$(curl "${args[@]}" -w '\n<<HTTP_STATUS>>%{http_code}\n')"
  status="$(printf '%s' "$out" | sed -n 's/.*<<HTTP_STATUS>>\([0-9]*\).*/\1/p' | tail -1)"
  printf '%s\n' "$out" > "$resp_file"
  log "  ← RESPONSE HTTP ${status}"
  printf '%s\n' "$out" | sed '/<<HTTP_STATUS>>/d' | head -c 1500 | sed 's/^/    /'
  LAST_BODY="$(printf '%s' "$out" | sed '/<<HTTP_STATUS>>/d')"
  LAST_STATUS="$status"
}

# psqlq <步骤号> <名字> <SQL>：执行 SQL 并原样落盘
psqlq() {
  local step="$1" name="$2" sql="$3"
  local f="$EVIDENCE_DIR/${step}-${name}-psql.txt"
  {
    echo "--- SQL ---"
    echo "$sql"
    echo "--- OUTPUT ---"
    psql "$PG_DSN" -c "$sql" 2>&1 | grep -v -e '^perl:' -e 'LANGUAGE =' -e 'LC_ALL =' -e 'LANG =' -e 'supported and installed' -e 'Falling back'
  } > "$f"
  log "  ⟢ psql ${name} → $f"
  sed 's/^/    /' "$f"
  LAST_PSQL_FILE="$f"
}

# jget <python 表达式>（对最近一次响应体求值；d = 已解析 JSON）
jget() { python3 -c 'import json,sys
try:
    d=json.load(sys.stdin)
except Exception:
    print(""); raise SystemExit
print(eval(sys.argv[1], {"d": d}))' "$1" <<<"$LAST_BODY" 2>/dev/null; }

log "=============================================================="
log " C8 复跑（整改后）关键项重验  D-CURSOR-030"
log " BASE_URL=$BASE_URL"
log " EVIDENCE_DIR=$EVIDENCE_DIR"
log " PG_DSN=$PG_DSN"
log "=============================================================="

# ---------------------------------------------------------------------------
# S0. 迁移快照（全新库 + 274 无 idle 报错）
# ---------------------------------------------------------------------------
log ""
log "[S0] 全新库迁移全量 apply 快照"
psqlq "S0" "schema_migrations_count" "SELECT count(*) AS applied, max(filename) AS max_filename FROM schema_migrations;"
psqlq "S0" "migrations_27x" "SELECT filename, left(checksum,16) AS checksum FROM schema_migrations WHERE filename LIKE '27%' ORDER BY filename;"
MIG_COUNT="$(psql "$PG_DSN" -tAc 'SELECT count(*) FROM schema_migrations;' 2>/dev/null | tr -d ' ')"
for m in 273_create_quota_keys.sql 274_add_quota_key_fk.sql 275_quota_keys_soft_delete.sql; do
  cnt="$(psql "$PG_DSN" -tAc "SELECT count(*) FROM schema_migrations WHERE filename='$m';" 2>/dev/null | tr -d ' ')"
  assert "S0 迁移 $m 已 apply（count=${cnt}）" "$([ "$cnt" = "1" ] && echo 0 || echo 1)" "S0-migrations_27x-psql.txt"
done
log "  schema_migrations 总数=${MIG_COUNT}"

# ---------------------------------------------------------------------------
# 整改 #1：契约（分页信封 items/total + code_masked + sweep {swept}）
# ---------------------------------------------------------------------------
log ""
log "[#1-a] GET /api/v1/admin/quota_keys → 分页信封(items/total) + code_masked"
http "10" "list_quota_keys" "GET" "/api/v1/admin/quota_keys"
LIST_IS_ARRAY="$(jget "isinstance(d.get('data',{}).get('items'), list)")"
LIST_TOTAL="$(jget "d.get('data',{}).get('total')")"
LIST_MASKED="$(jget "(d.get('data',{}).get('items') or [{}])[0].get('code_masked')")"
log "  items_is_list=${LIST_IS_ARRAY} total=${LIST_TOTAL} items[0].code_masked=${LIST_MASKED}"
assert "#1 GET 列表 data.items 为数组（分页信封）" "$([ "$LIST_IS_ARRAY" = "True" ] && echo 0 || echo 1)" "10-list_quota_keys-response.txt"
assert "#1 GET 列表 data.total 存在（${LIST_TOTAL}）" "$([ -n "$LIST_TOTAL" ] && [ "$LIST_TOTAL" != "None" ] && echo 0 || echo 1)" "10-list_quota_keys-response.txt"
assert "#1 GET 列表项含 code_masked（${LIST_MASKED}）" "$([ -n "$LIST_MASKED" ] && [ "$LIST_MASKED" != "None" ] && echo 0 || echo 1)" "10-list_quota_keys-response.txt"
assert "#1 GET 列表无激活码原文（F1）" "$(grep -q "$CODE_CAP" <<<"$LAST_BODY" && echo 1 || echo 0)" "10-list_quota_keys-response.txt"

log ""
log "[#1-b] 造过期 key → POST sweep_expired → 期望 data.swept 为数值且 ≥1"
http "11" "create_expired_key" "POST" "/api/v1/admin/quota_keys" \
  "{\"code\":\"${CODE_EXPIRED}\",\"expires_at\":\"2020-01-01T00:00:00Z\",\"notes\":\"C8 r2 fixture (expired)\"}"
EXPIRED_KEY_ID="$(jget "d.get('data',{}).get('id')")"
log "  EXPIRED_KEY_ID=${EXPIRED_KEY_ID}"
http "11" "sweep_expired" "POST" "/api/v1/admin/quota_keys/sweep_expired"
SWEPT="$(jget "d.get('data',{}).get('swept')")"
log "  swept=${SWEPT}"
assert "#1 sweep 响应含 swept 字段且为数值（${SWEPT}）" "$(printf '%s' "$SWEPT" | grep -qE '^[0-9]+$' && echo 0 || echo 1)" "11-sweep_expired-response.txt"
assert "#1 sweep 本轮改写 ≥1（swept=${SWEPT}）" "$([ -n "$SWEPT" ] && [ "$SWEPT" -ge 1 ] 2>/dev/null && echo 0 || echo 1)" "11-sweep_expired-response.txt"
http "11" "get_expired_key" "GET" "/api/v1/admin/quota_keys/${EXPIRED_KEY_ID}"
EXPIRED_STATUS="$(jget "d.get('data',{}).get('status')")"
assert "#1 过期 key 经 sweep 置 expired（实际 ${EXPIRED_STATUS}）" "$([ "$EXPIRED_STATUS" = "expired" ] && echo 0 || echo 1)" "11-get_expired_key-response.txt"

# ---------------------------------------------------------------------------
# 整改 #2 / ≤15：单 key 建号至 15 上限 → 第 16 个被拒
# ---------------------------------------------------------------------------
log ""
log "[#2] 单 key（ID=1）建号至 MaxAccountsPerQuotaKey=15 → 第 16 个被拒"
CAP_KEY_ID="$(psql "$PG_DSN" -tAc "SELECT id FROM quota_keys WHERE code_last4='0001' ORDER BY id LIMIT 1;" 2>/dev/null | tr -d ' ')"
log "  CAP_KEY_ID=${CAP_KEY_ID}"
WORKING_BEFORE="$(psql "$PG_DSN" -tAc "SELECT count(*) FROM accounts WHERE quota_key_id=${CAP_KEY_ID} AND deleted_at IS NULL AND status='active';" 2>/dev/null | tr -d ' ')"
log "  working(before)=${WORKING_BEFORE}"
i=$((WORKING_BEFORE + 1))
while [ "$i" -le 15 ]; do
  nm="$(printf 'r2-acc-%03d' "$i")"
  http "12" "create_account_${nm}" "POST" "/api/v1/admin/accounts" \
    "{\"name\":\"${nm}\",\"platform\":\"cursor\",\"type\":\"apikey\",\"credentials\":{\"api_key\":\"sk-r2-${nm}\"},\"quota_key_id\":${CAP_KEY_ID}}"
  i=$((i + 1))
done
psqlq "12" "accounts_under_cap_key" "SELECT count(*) AS working_accounts FROM accounts WHERE quota_key_id=${CAP_KEY_ID} AND deleted_at IS NULL AND status='active';"
WORKING_AFTER="$(psql "$PG_DSN" -tAc "SELECT count(*) FROM accounts WHERE quota_key_id=${CAP_KEY_ID} AND deleted_at IS NULL AND status='active';" 2>/dev/null | tr -d ' ')"
log "  working(after)=${WORKING_AFTER}"
assert "#2 单 key 工作账号恰好 15（实际 ${WORKING_AFTER}）" "$([ "$WORKING_AFTER" = "15" ] && echo 0 || echo 1)" "12-accounts_under_cap_key-psql.txt"

log ""
log "[#2-b] 第 16 个建号 → 期望被拒（上限拦截）"
http "12" "create_account_16th_rejected" "POST" "/api/v1/admin/accounts" \
  "{\"name\":\"r2-acc-016\",\"platform\":\"cursor\",\"type\":\"apikey\",\"credentials\":{\"api_key\":\"sk-r2-016\"},\"quota_key_id\":${CAP_KEY_ID}}"
REJECT_MSG="$(jget "d.get('message') or d.get('reason') or ''")"
log "  16th message=${REJECT_MSG}"
assert "#2 第 16 个建号被拒（HTTP ${LAST_STATUS}）" "$([ "${LAST_STATUS:-000}" -ge 400 ] && echo 0 || echo 1)" "12-create_account_16th_rejected-response.txt"
WORKING_FINAL="$(psql "$PG_DSN" -tAc "SELECT count(*) FROM accounts WHERE quota_key_id=${CAP_KEY_ID} AND deleted_at IS NULL AND status='active';" 2>/dev/null | tr -d ' ')"
ACC_016="$(psql "$PG_DSN" -tAc "SELECT count(*) FROM accounts WHERE name='r2-acc-016';" 2>/dev/null | tr -d ' ')"
log "  working(final)=${WORKING_FINAL} r2-acc-016 落库行数=${ACC_016}"
psqlq "12" "after_16th" "SELECT count(*) AS working_accounts FROM accounts WHERE quota_key_id=${CAP_KEY_ID} AND deleted_at IS NULL AND status='active'; SELECT count(*) AS acc016_rows FROM accounts WHERE name='r2-acc-016';"
assert "#2 被拒后仍为 15 且 16th 未落库（working=${WORKING_FINAL}, 016 行=${ACC_016}）" "$([ "$WORKING_FINAL" = "15" ] && [ "$ACC_016" = "0" ] && echo 0 || echo 1)" "12-after_16th-psql.txt"

# ---------------------------------------------------------------------------
# 整改 #3：cursor 账号编辑（不改归属）→ 成功
# ---------------------------------------------------------------------------
log ""
log "[#3] PUT /api/v1/admin/accounts/:id（cursor，请求体不含 quota_key_id）→ 成功且归属不变"
ACC_ID="$(psql "$PG_DSN" -tAc "SELECT id FROM accounts WHERE quota_key_id=${CAP_KEY_ID} AND deleted_at IS NULL ORDER BY id LIMIT 1;" 2>/dev/null | tr -d ' ')"
QK_BEFORE="$(psql "$PG_DSN" -tAc "SELECT quota_key_id FROM accounts WHERE id=${ACC_ID};" 2>/dev/null | tr -d ' ')"
log "  ACC_ID=${ACC_ID} quota_key_id(before)=${QK_BEFORE}"
http "13" "update_account_without_quota_key" "PUT" "/api/v1/admin/accounts/${ACC_ID}" \
  "{\"name\":\"r2-acc-001-edited\",\"notes\":\"edited without touching quota_key_id\"}"
QK_AFTER="$(psql "$PG_DSN" -tAc "SELECT quota_key_id FROM accounts WHERE id=${ACC_ID};" 2>/dev/null | tr -d ' ')"
NAME_AFTER="$(psql "$PG_DSN" -tAc "SELECT name FROM accounts WHERE id=${ACC_ID};" 2>/dev/null | tr -d ' ')"
psqlq "13" "account_after_edit" "SELECT id, name, platform, quota_key_id, deleted_at FROM accounts WHERE id=${ACC_ID};"
log "  quota_key_id(after)=${QK_AFTER} name(after)=${NAME_AFTER}"
assert "#3 cursor 账号编辑成功（HTTP ${LAST_STATUS}）" "$([ "$LAST_STATUS" = "200" ] && echo 0 || echo 1)" "13-update_account_without_quota_key-response.txt"
assert "#3 编辑后归属不变（${QK_BEFORE} → ${QK_AFTER}）" "$([ "$QK_BEFORE" = "$QK_AFTER" ] && echo 0 || echo 1)" "13-account_after_edit-psql.txt"
assert "#3 编辑生效（name=${NAME_AFTER}）" "$([ "$NAME_AFTER" = "r2-acc-001-edited" ] && echo 0 || echo 1)" "13-account_after_edit-psql.txt"

# ---------------------------------------------------------------------------
# 整改 #6：PUT expires_at 显式 null → 清空；省略 → 不变
# ---------------------------------------------------------------------------
log ""
log "[#6] quota_keys PUT expires_at 三态：null→清空 / 省略→不变 / 有值→设置"
http "16" "create_tristate_key" "PUT" "/api/v1/admin/quota_keys/${EXPIRED_KEY_ID}" \
  "{\"expires_at\":\"2030-01-01T00:00:00Z\"}"
log ""
log "[#6-a] 显式 null → 清空"
http "16" "expires_null" "PUT" "/api/v1/admin/quota_keys/${EXPIRED_KEY_ID}" '{"expires_at":null}'
EXP_AFTER_NULL="$(psql "$PG_DSN" -tAc "SELECT coalesce(expires_at::text,'NULL') FROM quota_keys WHERE id=${EXPIRED_KEY_ID};" 2>/dev/null | tr -d ' ')"
psqlq "16" "after_expires_null" "SELECT id, expires_at, (expires_at IS NULL) AS is_null FROM quota_keys WHERE id=${EXPIRED_KEY_ID};"
log "  expires_at(after null)=${EXP_AFTER_NULL}"
assert "#6 显式 null 清空 expires_at（实际 ${EXP_AFTER_NULL}）" "$([ "$EXP_AFTER_NULL" = "NULL" ] && echo 0 || echo 1)" "16-after_expires_null-psql.txt"

log ""
log "[#6-b] 有值 → 设置；随后省略 expires_at → 不变"
http "16" "expires_set" "PUT" "/api/v1/admin/quota_keys/${EXPIRED_KEY_ID}" '{"expires_at":"2031-06-01T00:00:00Z"}'
EXP_SET="$(psql "$PG_DSN" -tAc "SELECT coalesce(expires_at::text,'NULL') FROM quota_keys WHERE id=${EXPIRED_KEY_ID};" 2>/dev/null | tr -d ' ')"
log "  expires_at(after set)=${EXP_SET}"
http "16" "expires_omitted" "PUT" "/api/v1/admin/quota_keys/${EXPIRED_KEY_ID}" '{"notes":"omit expires_at probe"}'
EXP_OMIT="$(psql "$PG_DSN" -tAc "SELECT coalesce(expires_at::text,'NULL') FROM quota_keys WHERE id=${EXPIRED_KEY_ID};" 2>/dev/null | tr -d ' ')"
NOTES_OMIT="$(psql "$PG_DSN" -tAc "SELECT coalesce(notes,'NULL') FROM quota_keys WHERE id=${EXPIRED_KEY_ID};" 2>/dev/null)"
psqlq "16" "after_expires_omitted" "SELECT id, expires_at, notes FROM quota_keys WHERE id=${EXPIRED_KEY_ID};"
log "  expires_at(after omit)=${EXP_OMIT} notes=${NOTES_OMIT}"
assert "#6 省略 expires_at → 值不变（${EXP_SET} → ${EXP_OMIT}）" "$([ "$EXP_SET" = "$EXP_OMIT" ] && [ "$EXP_OMIT" != "NULL" ] && echo 0 || echo 1)" "16-after_expires_omitted-psql.txt"
assert "#6 省略 expires_at 时同请求 notes 仍生效（notes=${NOTES_OMIT}）" "$(grep -q 'omit expires_at probe' <<<"$NOTES_OMIT" && echo 0 || echo 1)" "16-after_expires_omitted-psql.txt"

# ---------------------------------------------------------------------------
# 整改 #7：ops_job_heartbeats 含 cursor_account_retention（跑一轮后）
# ---------------------------------------------------------------------------
log ""
log "[#7] ops_job_heartbeats 含 cursor_account_retention（启动延迟 5min，最长等待 ${HEARTBEAT_MAX_WAIT}s）"
WAITED=0
while [ "$WAITED" -lt "$HEARTBEAT_MAX_WAIT" ]; do
  HB_CNT="$(psql "$PG_DSN" -tAc "SELECT count(*) FROM ops_job_heartbeats WHERE job_name='cursor_account_retention';" 2>/dev/null | tr -d ' ')"
  if [ "$HB_CNT" = "1" ]; then break; fi
  sleep 20
  WAITED=$((WAITED + 20))
done
psqlq "17" "heartbeat_cursor_account_retention" "SELECT job_name, last_run_at, last_success_at, last_error, last_duration_ms, last_result, updated_at FROM ops_job_heartbeats WHERE job_name='cursor_account_retention';"
psqlq "17" "heartbeat_all_jobs" "SELECT job_name, last_run_at, last_success_at, updated_at FROM ops_job_heartbeats ORDER BY job_name;"
HB_CNT="$(psql "$PG_DSN" -tAc "SELECT count(*) FROM ops_job_heartbeats WHERE job_name='cursor_account_retention';" 2>/dev/null | tr -d ' ')"
HB_SUCCESS="$(psql "$PG_DSN" -tAc "SELECT coalesce(last_success_at::text,'NULL') FROM ops_job_heartbeats WHERE job_name='cursor_account_retention';" 2>/dev/null | tr -d ' ')"
log "  等待 ${WAITED}s；cursor_account_retention 心跳行数=${HB_CNT}；last_success_at=${HB_SUCCESS}"
assert "#7 ops_job_heartbeats 含 cursor_account_retention（行数=${HB_CNT}）" "$([ "$HB_CNT" = "1" ] && echo 0 || echo 1)" "17-heartbeat_cursor_account_retention-psql.txt"
assert "#7 该作业心跳已报成功（last_success_at=${HB_SUCCESS}）" "$([ "$HB_SUCCESS" != "NULL" ] && [ -n "$HB_SUCCESS" ] && echo 0 || echo 1)" "17-heartbeat_cursor_account_retention-psql.txt"

log ""
log "=============================================================="
log " 断言汇总：PASS=${PASS_COUNT}  FAIL=${FAIL_COUNT}"
log " 证据目录：$EVIDENCE_DIR"
log "=============================================================="
if [ "$FAIL_COUNT" -gt 0 ]; then
  exit 1
fi
exit 0
