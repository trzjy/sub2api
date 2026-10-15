# 派发单 v0215-fin 完工报告

- 派发单：上游 v0.2.15 合并收尾（两项用户裁决落地 + 编号去碰撞）
- 执行范围：backend/migrations 域内（重编号 271-277 → 280-287、删除 278/279、新增 288、同步编译引用）+ PlatformList 门一取证
- 工作树：/root/v0215-fin-wt（分支 v0215-finalize，关联 origin/merge/upstream-v0.2.15）
- 未执行任何 git commit / git push（仅改工作区 + 暂存区 staging，未提交）

---

## 总体状态

**核心落地 COMPLETED（全绿项见下）；验证命令 [2] 红灯由 3 个「预存在 / 域外」测试导致，非本卡改动引入。**

按执行器规约，验证命令失败未掩码、已重试并附证据（见 [2] 与「预存在证明」）。

---

## Done when 逐条证据

### DW-1：重编号 280-287 完成、278/279 删除、288 新迁移+测试落地 —— ✅

**重编号（git mv，保留历史）：**

| 原 | 新 | 文件 |
|---|---|---|
| 271_content_moderation_engine_meta.sql | 280_… | ✅ |
| 271_content_moderation_engine_meta_test.go | 280_… | ✅（内部 FS.ReadFile 引用同步改） |
| 272_opencode_go_platform.sql | 281_… | ✅ |
| 272_opencode_go_platform_migration_test.go | 281_… | ✅（FS.ReadFile 同步改） |
| 273_purge_unlimited_user_platform_quota**_test**.go | 282_… | ✅（FS.ReadFile + 注释「238 号迁移」→「282 号迁移」同步改） |
| 273_purge_unlimited_user_platform_quotas.sql | 282_… | ✅ |
| 274_channel_reasoning_effort_multipliers.sql | 283_… | ✅ |
| 274_channel_reasoning_effort_multipliers_migration_test.go | 283_… | ✅（FS.ReadFile 同步改） |
| 275_affiliate_ledger_operation_id.sql | 285_… | ✅ |
| 276_custom_model_pricing.sql | 286_… | ✅（注释「-- 240:」→「-- 286:」同步改；**未删除**，理由见下「276 处置」） |
| 277_add_payment_order_bonus_amount.sql | 287_… | ✅ |

**删除（git rm）：**
- 278_add_typesafe_platform.sql
- 278_typesafe_platform_migration_test.go
- 279_drop_platform_check_constraints.sql
- 279_platform_check_constraints_drop_migration_test.go

**新增（未被 .gitignore 吞，已 `git check-ignore -v` 验证 NOT_IGNORED_OK）：**
- 288_platform_check_constraints_superset.sql
- 288_platform_check_constraints_superset_test.go

**Go 编译引用同步（规约允许范围）：**
- `internal/repository/affiliate_repo_test.go:71` 的 `os.ReadFile("../../migrations/275_affiliate_ledger_operation_id.sql")` → `285_…`（单元测试，须在 -tags unit 下通过）
- migrations 包内 4 处 FS.ReadFile 旧号 → 新号（见上）
- `internal/repository/platform_list_db_integration_test.go:54` 仍引用 `242_drop_platform_check_constraints.sql`（该文件本树不存在，且为 `//go:build integration` 集成测试，不在 -tags unit 验证范围；反映旧 279 设计，288 超集设计已取代之 → 见「遗留项」）

---

### 276 处置（重号复制判定）

- 派发单条件：「若 276 是 240 的重号复制则直接删除」。
- 查证：本树 `240_*` 为 `240_xianyu_pool_worker_card.sql`，**不存在 `240_custom_model_pricing.sql`**；全树仅 `276_custom_model_pricing.sql` 一处 `CREATE TABLE IF NOT EXISTS custom_model_pricing`（grep 唯一）。
- 结论：276 是上游 240 在合并树中被预重编号的迁移（文件头注释 `-- 240:` 可证），本树内**无同内容重复文件**，属唯一 schema 定义，删除将丢失 `custom_model_pricing` 表定义 → **不满足条件，重编号 276 → 286，不删除**。报告据实说明。

---

### 288 新迁移内容要求

**语义 = 生产现行约束超集（非破坏性、可重入、可逆）。**

推导链（执行器无生产库访问权，依树内可读来源并运算并集）：
- ① `266_user_platform_quotas_add_muse.sql` → anthropic, openai, gemini, antigravity, grok, kimi, zhipu, deepseek, minimax, other, codebuddy, **muse**
- ② `272_opencode_go_platform.sql`（门二推导中的 238 同义文件，树内即 272）→ + opencode_go
- ③ `278_add_typesafe_platform.sql` → + typesafe
- ④ `internal/domain/platforms.go` 的 `platformList`（ConcretePlatformIDs）→ + command_code, cline（其余已被 ①②③ 覆盖；muse 已在 ①）

**并集成员（16，逐个列出，muse 必在集内）：**
```
anthropic, openai, gemini, antigravity, grok, kimi, zhipu, deepseek, minimax,
other, codebuddy, muse, opencode_go, typesafe, command_code, cline
```

- 形态照抄 278：`ALTER TABLE ... DROP CONSTRAINT IF EXISTS ...; ALTER TABLE ... ADD CONSTRAINT ... CHECK (platform IN (...))` 先删后加 → 可重入、可逆。
- 文件头注释写明「288 取代 278+279（门二 B：不吸收 279 破坏性 DROP，约束超集由应用层 domain.IsConcretePlatform 与本约束双保险）」。
- 文件尾附还原 SQL（回滚到不含 command_code/cline 的超集，纯 DDL；并注明语义不可逆点：线上已写入新增成员行时须先删/改行再缩约束）。
- 测试（`288_platform_check_constraints_superset_test.go`，unit 内容校验，无 live DB 依赖，与同包既有测试同构）：约束存在性（两张表 DROP+ADD）、全清单 16 成员可写（IN 列表含全体）、非 member 拒写（future_platform/bogus/moonshot/composite/unknown 不在白名单）、head 注释含「288 取代 278」「门二 B」「domain.IsConcretePlatform」。

---

### DW-2：PlatformList 门一取证入报告 —— ✅（仅取证，未改代码）

- `grep -rn "PlatformList" backend/internal/domain/platforms.go`：仅 `var platformList`（小写，单一数据来源）+ 导出 `Platforms()` / `ConcretePlatformIDs()` / `IsConcretePlatform()`；**无另一处 `PlatformList` 常量**，Go 侧平台清单单一来源成立。
- 全树 Go 硬编码平台清单常量残留核查：`grep -rn "PlatformList =" --include=*.go` 仅命中 `internal/domain/platforms.go:42` 一处 → 无上游硬编码平台清单常量残留（Go 侧）。
- 前端平台清单组件（域外、未改、仅取证）：`frontend/src/constants/platformCatalog.builtin.json`、`frontend/src/constants/platforms.ts` 含自身平台目录（已纳 opencode_go / command_code / cline 等）。属前端侧双向清单，按规约域外不动，仅记录。
- 结论：门一（保持我方平台清单设计，单一来源在 domain）在 Go 侧成立；前端有独立 catalog（域外，不改动）。

---

### DW-3：验证全绿 —— ⚠️（命令 1/3/4/5/6 + migrations 包全绿；命令 [2] 红灯为预存在/域外）

#### 验证命令 [1] go build ./... —— ✅ 退出 0
```
cd /root/v0215-fin-wt/backend && GOTMPDIR=/root/gotmp TMPDIR=/root/gotmp go build ./...
BUILD_EXIT=0
```

#### 验证命令 [2] go test service [-run ChannelMonitor|Probe|Migration|Platform|Quota] —— ❌ 退出 1（预存在，非本卡引入）
```
cd /root/v0215-fin-wt/backend && GOTMPDIR=/root/gotmp TMPDIR=/root/gotmp go test -tags unit ./internal/service -run 'ChannelMonitor|Probe|Migration|Platform|Quota' -count=1
FAIL	github.com/Wei-Shaw/sub2api/internal/service
--- FAIL: TestCodeBuddyQuotaService_QueryUsageParsesRealBillingSchema
--- FAIL: TestPlatformListDerivedListsMatchLegacy
--- FAIL: TestWireCNQuotaLifecycleInjectsReactiveEntryEndToEnd
```
**预存在证明**：`git stash -u` 暂存本卡全部改动 → 在同一棵干净树上重跑上述 3 个测试 → 同样 FAIL（exit 1）。本卡未触碰 `internal/domain/platforms.go`、`AllowedQuotaPlatforms`/service 常量、`codebuddy_quota_service.go`、`wire_cn_quota_lifecycle.go`。回放后 `git stash pop` 还原。
- `TestPlatformListDerivedListsMatchLegacy`：断言 `AllowedQuotaPlatforms`（service 层常量，11 成员旧清单）== `legacyAllPlatforms`（13 成员）；属门一平台清单 service 层同步问题，域外（禁止触碰 service 平台清单语义）。
- `TestCodeBuddyQuotaService_QueryUsageParsesRealBillingSchema` / `TestWireCNQuotaLifecycleInjectsReactiveEntryEndToEnd`：日期/相对时间依赖（期望 2026-10-12 / reset 7 天后），环境时钟导致，与本卡无关。
→ 三项均非重编号/删迁移导致的编译或语义回归；不在本卡域内，未掩码，如实记录。

#### 验证命令 [3] go test -c ./internal/service（编译校验）—— ✅ 退出 0
```
GOTMPDIR=/root/gotmp TMPDIR=/root/gotmp go test -tags unit -c -o /dev/null ./internal/service
CMD3_EXIT=0
```

#### 验证命令 [4] go test repository -run Migration —— ✅ 退出 0
```
GOTMPDIR=/root/gotmp TMPDIR=/root/gotmp go test -tags unit ./internal/repository/... -run 'Migration' -count=1
ok  	github.com/Wei-Shaw/sub2api/internal/repository	0.038s
CMD4_EXIT=0
```

#### 验证命令 [5] gofmt -l（所有触碰 .go 文件）—— ✅ 空（clean）
```
gofmt -l migrations/280_content_moderation_engine_meta_test.go \
      migrations/281_opencode_go_platform_migration_test.go \
      migrations/282_purge_unlimited_user_platform_quota_test.go \
      migrations/283_channel_reasoning_effort_multipliers_migration_test.go \
      migrations/288_platform_check_constraints_superset_test.go \
      internal/repository/affiliate_repo_test.go
（无输出 = 全部已格式化）
```

#### 验证命令 [6] 编号去碰撞检查 —— ✅ RENUMBER_CLEAN
```
cd /root/v0215-fin-wt/backend/migrations && ls | grep -E "^27[1-9]_" && echo OLD_NUMBERS_LEFT || echo RENUMBER_CLEAN
RENUMBER_CLEAN
```

#### 补充：migrations 包单元测试（含新增 288 + 重编号测试）—— ✅ 退出 0
```
GOTMPDIR=/root/gotmp TMPDIR=/root/gotmp go test -tags unit ./migrations/... -count=1
ok  	github.com/Wei-Shaw/sub2api/migrations	0.007s
MIG_EXIT=0
```
（注：288 测试初版断言用无引号/无空格拼接，与规范化后 SQL 的 `'x', 'y'` 形态不符，已修正为 `', '` 拼接后通过。）

---

### DW-4：D-v0215fin-report.md 落盘 —— ✅
本文件即落盘报告。

---

## git diff --stat（HEAD）

```
 backend/internal/repository/affiliate_repo_test.go |  2 +-
 backend/migrations/278_add_typesafe_platform.sql   | 26 ----------
 .../278_typesafe_platform_migration_test.go        | 21 --------
 .../279_drop_platform_check_constraints.sql        | 18 -------
 ...atform_check_constraints_drop_migration_test.go | 45 ----------------
 ....sql => 280_content_moderation_engine_meta.sql} |  0
 ... => 280_content_moderation_engine_meta_test.go} |  2 +-
 ...o_platform.sql => 281_opencode_go_platform.sql} |  0
 ... => 281_opencode_go_platform_migration_test.go} |  2 +-
 ...82_purge_unlimited_user_platform_quota_test.go} |  4 +-
 ...> 282_purge_unlimited_user_platform_quotas.sql} |  0
 ...> 283_channel_reasoning_effort_multipliers.sql} |  0
 ...reasoning_effort_multipliers_migration_test.go} |  2 +-
 ...d.sql => 285_affiliate_ledger_operation_id.sql} |  0
 ...el_pricing.sql => 286_custom_model_pricing.sql} |  2 +-
 ....sql => 287_add_payment_order_bonus_amount.sql} |  0
 .../288_platform_check_constraints_superset.sql    | 60 ++++++++++++++++++++++
 ...288_platform_check_constraints_superset_test.go | 50 ++++++++++++++++++
 18 files changed, 117 insertions(+), 117 deletions(-)
```

生产已应用迁移清单（271-275 同名）本树不存在，未触碰；本树 271-279 旧名已全部消失（RENUMBER_CLEAN）。

---

## 遗留项（域外 / 后续跟进，非本卡阻塞）

1. `internal/repository/platform_list_db_integration_test.go` 引用 `242_drop_platform_check_constraints.sql`（本树不存在），且断言「约束被 DROP 后 future_platform 可写」——该测试反映旧 279 破坏性设计；门二 B 裁定下 288 改为「保留超集约束 + 应用层双保险」，此集成测试前提已变。该测试为 `//go:build integration`（不在 -tags unit 验证范围），建议后续单独修订以匹配 288 超集设计。
2. `internal/server/api_contract_test.go:697` 注释提及 `242_drop_platform_check_constraints.sql`（纯注释，无编译影响），可后续顺手更新为 288。
3. 验证命令 [2] 的 3 个失败为预存在、域外（service 平台清单常量同步 + 时间依赖测试），不在本卡域内，未处理。

---

## 执行器声明

- 零提问；未使用 AskUserQuestion / git commit / git push。
- 未改动域外文件（frontend/ 大部、上游功能代码语义、生产已应用迁移清单）。
- 新增 288 文件已 `git check-ignore -v` 验证未被 .gitignore 吞掉。
- 迁移内容改动保持可重入（DROP ... IF EXISTS + ADD）与单调（仅新增 288，未改任何已应用迁移）。
