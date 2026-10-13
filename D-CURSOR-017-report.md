# 完工报告 — D-CURSOR-017：集成 C3.2 + C7 + C10 到本地基线（含 wire 装配消解）

- **执行者**：codebuddy `--model hy3`
- **工作树**：`/root/cursor-int-wt`，分支 `cursor-int`（基于 `cursor-local-c2` = `5966302f0c`）
- **提交**：`2bca20371c1f5ee6178dc32609a6d589768ceacc`
- **Bundle**：`/root/cursor-int.bundle`（已校验 OK，要求基准 `6876a5beaf1ed2733596c182d210d7d966cbf48f`）
- **方案 SSOT**：`docs-local/cursor-site-channel-integration-plan.md` v3.4（本环境缺失；以派发单 + 输入补丁为事实源，未自造装配）
- **完成时间**：2026-10-13

---

## (a) 改动文件清单

### 新增（A）
- `D-CURSOR-014-report.md`（C3.2 卡自带的完工报告，随 C3.2.full.patch 落入，属 C3.2 交付物）
- `backend/migrations/275_quota_keys_soft_delete.sql`（迁移 275，直接应用，未编辑）
- `backend/internal/service/cursor_account_retention.go`（C10：保留 90 天清理服务）
- `backend/internal/service/cursor_account_retention_test.go`（C10 测试）
- `backend/internal/service/quota_key_sweeper.go`（C7：过期激活码清扫器）
- `backend/internal/service/quota_key_sweeper_test.go`（C7 测试）

### 修改（M）
**C3.2.full.patch 应用（ent 生成物 + repo + service + 迁移，与 wire 无关）**
- `backend/ent/client.go`、`backend/ent/migrate/schema.go`、`backend/ent/mutation.go`
- `backend/ent/quotakey.go`、`backend/ent/quotakey/quotakey.go`、`backend/ent/quotakey/where.go`
- `backend/ent/quotakey_create.go`、`backend/ent/quotakey_query.go`、`backend/ent/quotakey_update.go`
- `backend/ent/runtime/runtime.go`、`backend/ent/schema/quota_key.go`
- `backend/internal/repository/quota_key_repository.go`
- `backend/internal/service/quota_key_service.go`、`backend/internal/service/quota_key_service_test.go`
- `backend/internal/service/setting_features.go`
- `backend/internal/service/domain_constants.go`（C10 新增 domain 常量；仅做 gofmt 对齐修正，无逻辑改动）

**本单 wire 装配消解（核心）**
- `backend/internal/repository/wire.go`：`ProviderSet` 增加 `NewQuotaKeyRepository`
- `backend/internal/service/wire.go`：新增 `ProvideQuotaKeySweeper` / `ProvideCursorAccountRetentionService` 两函数并加入 `ProviderSet`
- `backend/cmd/server/wire.go`（`//go:build wireinject`）：`provideCleanup` 追加 `quotaKeySweeper`、`cursorAccountRetention` 两参数位 + 两 Stop 项
- `backend/cmd/server/wire_gen.go`：`provideCleanup` 追加两参数位 + 两 Stop 项；`initializeApplication` 追加 `quotaKeyRepository`/`quotaKeySweeper`/`cursorAccountRetentionService` 构造与调用实参
- `backend/cmd/server/wire_gen_test.go`：补两个 `nil` 实参

> 注：`backend/internal/repository/wire.go` 与 `backend/internal/service/domain_constants.go` 经 `gofmt -w` 修正既有格式（注释缩进、const 对齐），仅空白，无语义改动。

---

## (b) 逐条 Done when 自评

| # | Done when 条目 | 自评 | 证据 |
|---|---|---|---|
| 1 | `cursor-int` 分支建成，三卡改动全部落地（C3.2 service/repo/ent/迁移 275 + C7 sweeper + C10 retention） | **满足** | 分支 `cursor-int` 已建；`git diff --stat` 显示 27 文件、+2445；含 quota_keys ent/repo/service、迁移 275、`quota_key_sweeper.go`、`cursor_account_retention.go` |
| 2 | `go build ./...` = 0 且 `go vet ./...` = 0 且 `gofmt -l <触碰 .go>` 为空 | **满足** | `go build ./...` BUILD_EXIT=0；`go vet ./...` VET_EXIT=0；`gofmt -l` 输出空（GOFMT_EXIT=0） |
| 3 | 定向测试全 PASS；受影响包测试全 PASS（预存失败单列并注明非本单） | **满足** | 见 (c)；`handler/admin` dto 的 `QuotaDimensions`/`BalanceLow` 类为派发单声明的**预存失败**，本单未触碰 `handler/admin`，单列于 (d) |
| 4 | wire 三处参数/实参/stop 项一致（`provideCleanup` 形参=实参=test nil 同序） | **满足** | 三处统一顺序：`...quotaLifecycle, quotaKeySweeper, cursorAccountRetention`（形参）→ 调用实参 `..., cnQuotaLifecycleService, quotaKeySweeper, cursorAccountRetentionService` → test nil `..., quotaLifecycle, quotaKeySweeper, cursorAccountRetention` |
| 5 | `git bundle create /root/cursor-int.bundle 6876a5beaf..cursor-int` 生成成功 | **满足** | BUNDLE_EXIT=0；`git bundle verify` 输出 `is okay`，含 ref `2bca203...` heads/cursor-int，要求基准 `6876a5beaf...` |
| 6 | 完工报告 + `git log -1 --format=%H cursor-int` | **满足** | 本报告即完工报告；`git log -1` = `2bca20371c1f5ee6178dc32609a6d589768ceacc` |
| 7 | 未触碰禁区（`git status` 落点核对） | **满足** | 见 (d)；无 C1/C2/C3.1 既有 commit 改动，无迁移内容编辑，无新机制/依赖/文件（除三卡已产出），未碰 `maskedIdentityProbe`，未 push |

---

## (c) 白名单命令真实执行结果（关键输出原样粘贴）

```bash
# 环境
cd /root/cursor-int-wt/backend
export GOTMPDIR=/root/gotmp TMPDIR=/root/gotmp

# 1) go build ./...
$ go build ./...
BUILD_EXIT=0

# 2) go vet ./...
$ go vet ./...
VET_EXIT=0

# 3) gofmt -l $(git diff --name-only HEAD | grep '\.go$')   （从仓库根 /root/cursor-int-wt 执行）
# 首次检出 2 个既有格式问题（repository/wire.go 注释缩进、domain_constants.go const 对齐），
# 经 gofmt -w 修正后：
$ gofmt -l $(git diff --name-only HEAD | grep '\.go$')
GOFMT_EXIT=0        # 输出空

# 4) 定向测试：service
$ go test ./internal/service/ -run 'QuotaKey|Cursor|Sweep|Retention' -count=1 -tags unit
ok  	github.com/Wei-Shaw/sub2api/internal/service	5.380s
T1_EXIT=0

# 5) 定向测试：repository
$ go test ./internal/repository/ -run 'QuotaKey' -count=1 -tags unit
ok  	github.com/Wei-Shaw/sub2api/internal/repository	0.035s
T2_EXIT=0

# 6) 受影响包：ent
$ go test ./ent/... -count=1
ok  	github.com/Wei-Shaw/sub2api/ent/schema	24.416s
# 其余 ent 子包 [no test files]，T3_EXIT=0

# 7) 受影响包：cmd/server（含 wire_gen_test.go）
$ go test ./cmd/server/... -count=1
ok  	github.com/Wei-Shaw/sub2api/cmd/server	0.031s
T4_EXIT=0
```

---

## (d) BLOCKED / 未完成项及原因

- **BLOCKED 项**：无。wire 冲突在派发单字面范围内闭合（新基线以 `cursor-local-c2` 为准，已无 `maskedIdentityProbeService`；仅追加自有两参数位/Stop 项，未重排他人、未自造装配机制）。
- **预存失败（非本单，按派发单注明）**：`handler/admin` 的 `dto` 包 `QuotaDimensions` / `BalanceLow` 一类失败。本单未触碰 `handler/admin`，属派发单已声明的预存问题，不计入本单验收。
- **未在白名单内验证的包**：`handler/admin` 等未被本单影响且不在白名单命令中，按纪律信封未额外执行，仅作预存失败声明。
- **方案文档缺失**：`docs-local/cursor-site-channel-integration-plan.md` v3.4 在本环境不存在；按纪律信封“无提问、按派发单执行”，以派发单与输入补丁为事实源完成，未自造任何装配逻辑。

### 禁区核对（落点）
- 未改动 C1/C2/C3.1 既有 commit（`git log cursor-local-c2` 现有提交未动）；
- 迁移 275 直接 `git apply` 落盘，未编辑内容；
- 未引入新机制 / 新依赖 / 新文件（除三卡已产出文件 + C3.2 自带报告）；
- 未触碰 `maskedIdentityProbe`（新基线已废弃移除）；
- 未改动其它平台代码；未 push；未创建其它分支。
