# B3a 收口方案 —— 影子新建入口封死剩余项（v16 §3.1/§3.2-1）

> 权威：`codebuddy-cockpit-fusion-plan.md` v16 §3.1（B3a 冻结）、§3.2-1（删除前引用清单
> B3a 起建）、§3.4（机制层保留）、§5 B3a 验收行。本方案零新增语义，仅登记 v16 已钉死
> 语义的落地缺口与完成判据；不另立行为条款。

更新：2026-09-29。

## 1. 现状取证（B3a 已落地部分）

| v16 条款 | 状态 | 证据 |
|---|---|---|
| §3.1-2 领域写入边界统一拒绝 | ✅ 已合入 | `enforceCodeBuddyShadowFreeze`（admin_account.go:1720），CreateAccount:794 / UpdateAccount:1174 挂载；CreateShadow 对 codebuddy 母账号早退返回 `CODEBUDDY_SHADOW_CREATION_FROZEN`；提交 `7f12c556b` |
| §2.1-4 桥接扩展（通用平台判定 ∪ 旧影子判定） | ✅ 已合入 | openai_gateway_forward.go:189；提交 `644de818a`（s4 终审整改，v16 修正版） |
| §3.1-3 拒绝回归测试 | ✅ 已合入 | codebuddy_shadow_freeze_test.go、admin_account_codebuddy_shadow_test.go、admin_service_spark_shadow_test.go |
| §3.1 删除 CreateShadow codebuddy 分支 + 前端向导入口 | ❌ 缺口 | 见 §2 |
| §3.1-1 全写入者盘点落盘 | ❌ 缺口（盘点已完成，未落盘） | 见 §3 与证据 01 |
| §3.2-1 删除前引用清单落盘 | ❌ 缺口 | B3a 起建，B3b 结束冻结 |

## 2. 缺口一：新建入口残留（v16 §3.1"删除…分支与前端向导入口"）

实现只做了"冻结"（边界拒绝 + 死代码保留），未做 v16 明文要求的"删除"：

1. **后端死分支**：`CreateShadow`（admin_account.go:1744）内 `isCodeBuddyParent`
   早退之后的 codebuddy 专属代码全部不可达（约 15 处交织点：platform 推断、
   `inferCodeBuddyShadowPlatform`、`defaultCodeBuddyShadowModelMapping`、
   ShadowOptions.Platform）。**冻结早退必须保留**（删除分支后它是 codebuddy 母账号
   建影子的唯一拒绝点，防落入 spark 分支建出错误维度影子）；spark 逻辑原样保留
   （§3.4）。随生产调用消灭，`inferCodeBuddyShadowPlatform` /
   `defaultCodeBuddyShadowModelMapping`（shadow_routing.go，仅新建链引用）一并删除，
   对应新建行为测试删除；**运行时影子路由/分发代码与测试一概不动**（B3c 范围）。
2. **前端向导入口**：`CodeBuddyShadowWizard.vue` + 挂载点（AccountsView.vue:534）+
   `__tests__/CodeBuddyShadowWizard.spec.ts` + `createCodeBuddyShadow` /
   `CodeBuddyShadowCreatePayload`（api/admin/accounts.ts:1076）+
   `listAccountShadows`（仅向导消费，随向导删除）。后端 `GET /shadows` 端点为
   运行时展示链，保留（B3c 范围）。

## 3. 缺口二：§3.1-1 全写入者盘点（主会话已完成核查，落盘证据 01）

结论摘要（逐项证据见 `~/.sub2api-acceptance/sub2api-b3a-freeze-20260929/01-writer-inventory.md`）：

- **经过领域边界**：admin CreateAccount / UpdateAccount（enforce 挂载）、
  CreateShadow（冻结早退）。
- **绕过边界但组合不可达**（已证明不受影响）：account_service.Create/Update/UpdateStatus
  （请求结构无 Platform 写回/无 ParentAccountID 字段）、admin BulkUpdate（字段集无
  platform/parent）、全部 UpdateExtra 调用点（仅 JSONB extra 合并，身份是独立列）、
  crs_sync_service 6×Create/Update（platform 固定 anthropic/openai/gemini，无
  ParentAccountID）、cockpit import CommitCockpitImport（repo upsert 无
  ParentAccountID）、web_zhipu 凭证回写（仅 Credentials）。无需收敛改造。
- **结论**：不存在未盘点而可达 `codebuddy+IsShadow` 的写入路径；边界单点封死成立。

## 4. 派发

| 卡 | 范围 | 执行 |
|---|---|---|
| b3a-card-a | 后端 CreateShadow codebuddy 分支删除 + 新建链专属函数/测试删除 | hy3 |
| b3a-card-b | 前端向导入口删除 | hy3 |
| （主会话直派 general-purpose） | §3.2-1 删除前引用清单扫描落盘 | 只读扫描 |

禁区（两卡共用）：`isCodeBuddyShadowAccount`/影子运行时路由/RPM 归母/凭证跳过/
spark 全链路零改动；`codebuddy_shadow_freeze_test.go` 与冻结拒绝测试保留；
存量影子其它字段更新链零改动。

## 5. Done when（B3a 验收，v16 §5 B3a 行）

1. 新建入口 grep 归零：全仓无 codebuddy 影子新建调用（唯一残留 = 领域边界拒绝点
   本身）；`createCodeBuddyShadow`、`CodeBuddyShadowWizard`、
   `inferCodeBuddyShadowPlatform`、`defaultCodeBuddyShadowModelMapping`、
   `ShadowOptions.Platform` 全仓零引用。
2. 存量影子运行时全在位：isCodeBuddyShadowAccount 六处调用点原样；影子路由/RPM/
   凭证跳过测试绿。
3. 定向验证：`go build ./...` + `go test -tags unit ./internal/service/ -run
   'Shadow|CodeBuddy' -v` 真实执行（防 -tags 静默空跑）；前端 type-check + 相关
   spec 通过。
4. 证据落盘：盘点（01）、删除前引用清单（02，B3b 结束冻结）、验收（03）于
   `~/.sub2api-acceptance/sub2api-b3a-freeze-20260929/`。
5. 终审（闸②）通过——账号身份写入链属高风险边界。
