# codebuddy 融合批次进度与接力文档

> 用途：跨会话接力。批次定义与权威语义见 `codebuddy-cockpit-fusion-plan.md` v16；
> 本文只记执行状态、证据位置、下一步。更新本文时无需改融合方案正文。

更新：2026-09-29（B3a 代码侧闭环）。

## 批次状态总览

| 批次 | 状态 | 说明 |
|---|---|---|
| B1（Cockpit 导出导入） | 🟡 卡在用户侧 | 代码已合入（早期波次）；**mapper 未实现**（§1.2 门控，等真实导出 fixture）；等用户提供 Cockpit 明文导出文件 → 脱敏归档+哈希登记 → mapper 派发 |
| B2（聚合直绑） | ✅ 代码侧闭环 | 提交 `51e841ab8`；**生产激活=独立切换门槛未执行**（见下） |
| B3a（影子新建入口封死） | ✅ 代码侧闭环 | 早期已合边界冻结 `7f12c556b` + 桥接 `644de818a`；本次收口删除新建分支与前端向导（终审 approve）；证据 `~/.sub2api-acceptance/sub2api-b3a-freeze-20260929/` |
| B3b（manifest 迁移） | ⬜ 未开始 | B2 → B3a → B3b → B3c 顺序；前置=遥测基线证明 + **B2 生产激活**；删除前引用清单已起建（02 文档，B3b 结束冻结） |
| B3c | ⬜ 未开始 | |
| B4 | 🔵 闸①通过（R8 approve，8 轮直连外审收敛），批1 A/D/E 三卡已并行派发 codebuddy | 探针已取证（2026-09-29，cn 站真实账号 HTTP 200，证据 `~/.sub2api-acceptance/sub2api-b4-probe-20260929/`）；§4.1 addendum 已回写（规范单位=credits、数值类型=*Precise→NUMERIC(20,8)、字段映射钉死）；**用户裁定 2026-09-29：expires_at = CycleEndTime（Asia/Shanghai 解析，Status=3 剔除）**——§4.2 冻结完全解除，B4 实现可按七步流程开工 |

## B3a 收口记录（2026-09-29）

- 方案：`docs/codebuddy-b3a-closure-plan.md`（零新增语义，权威=v16 §3.1/§3.2-1/§3.4）。
- 早期已合入：领域边界冻结 `enforceCodeBuddyShadowFreeze`（`7f12c556b`）、桥接扩展
  通用平台判定 ∪ 旧影子判定（`644de818a`，v16 s4 终审修正版）。
- 本次收口（7 卡，全部 codebuddy hy3）：CreateShadow codebuddy 死分支删除（冻结早退
  保留、spark 分支内联等价）、ShadowOptions/CreateShadowRequest 缩减为 spark 语义、
  新建链专属函数删除（inferCodeBuddyShadowPlatform/defaultCodeBuddyShadowModelMapping/
  shadowTargetsModel）、前端向导入口链删除（Wizard 组件+挂载+API+菜单按钮+i18n 43+13 死键）、
  发布 dist 重建归零验证。
- 终审：gpt-5.6-sol **approve 无阻断**（`~/.codex-companion/b3a-final-review/`；
  P2 dist 重建采纳、P3 i18n 逐键取证采纳，7 存活键保留——存量影子展示运行时链）。
- 验证：`go build` OK；`-tags unit` 定向 209 用例 PASS；前端 vue-tsc/vitest/`pnpm build` 全绿；
  新建入口归零 grep 零命中（dist gitignore 产物重建后亦零）。
- B3a 验收判据（v16 §5）：新建入口 grep 归零 ✅；存量影子运行时全在位 ✅。

## B2 闭环记录

- 方案：`docs/codebuddy-b2-aggregate-switch-plan.md`（四轮方案审收敛，gpt-5.6-sol）。
- 外审证据：`~/.codex-companion/b2-plan-review/`（方案审 r1-r4，终轮 approve）+
  `~/.codex-companion/b2-final-review/`（终审 approve，2×P2 已处置）。
- 派发单：`.omc/plans/b2-card-a.md`、`b2-card-b.md`（串行执行，hy3）。
- 关键设计：分组级开关 `aggregate_codebuddy_enabled`（默认 false）+ 263 迁移；
  repo 唯一纳入点同查询边界读分组权威值（失败关闭）；标准模式未分组桶失败关闭、
  simple 模式不变（激活拓扑外）；快照失效复用 GroupChanged outbox + 分组租约 +
  epoch 写令牌，零新机制；接口签名零改动。
- 验证：默认全量 + `-tags unit` 全量双套件零 FAIL；矩阵/翻转/收敛/读链集成用例齐。

## B2 生产激活门槛（独立动作，未执行）

前置 = B1 导入真实账号（**当前阻塞**）。切换清单（方案 §1.1/§5）：
1. 263 迁移实库升级 + 回滚验证（先迁移后发布；回滚保留列，禁 DROP）；
2. 取证：可调度 codebuddy 账号数=0（非零阻断）、受影响聚合分组清单与候选池、
   生产 runMode≠simple；
3. 逐分组：先解冻账号（既有管理端状态链，失败关闭）→ 后开绑定 → 候选池证据落盘；
4. 可逆停用演练一次 + 48h 观察段。

## 全局遗留项（不阻塞代码侧，完成声明需如实带出）

1. B1 mapper 未实现（§1.2 门控；无 fixture 禁止凭猜测实现）。
2. DB 集成测试需有 DB 环境：
   `go test -tags integration ./internal/repository/ -run TestAggregateBindingSuite -v`；
   `go test -tags integration ./internal/repository/ -run TestCockpitImportCommitIntegration -v`。
3. 部署侧网关/代理 body 日志审计（§1.7 纵深）待运维。
4. preview_parse_timeout 解码阶段取消用例补验。
5. 仓储复用事务分支提交锁（mapper 交付若获得调用方须先补）。

## 接力操作要点（下次会话直接用）

- 工作流：全局 skill `multi-agent-dispatch`（七步；方案审+终审走
  `node /mnt/data/.claude/plugins/oh-my-claudecode/scripts/codex-companion.mjs
  review --wait --scope task-snapshot --task-snapshot-file <自包含快照>`）。
  **working-tree scope 不可用于含 docs/.codex/.omc 材料的审查**（.gitignore 吞掉
  导致空 diff，B2 方案审一轮踩过）。
- 执行派发：`/home/zjy/.local/bin/codebuddy --model hy3 -p "<派发单路径+硬约束>"`，
  后台运行；额度耗尽切 `custom-local:deepseek-v4.1-flash`。
- **测试白名单必须带 `-tags unit`**（service/repository 多数测试文件带 unit tag，
  不带会静默空跑）；验收抽查 `-v` 确认用例真实执行。
- 主会话禁用 plan 模式；执行会话禁止 git 写操作；提交由主会话做
  （Co-Authored-By: Claude Code <noreply@anthropic.com>；docs/ 新文件 `git add -f`）。
- 派发通道账号事实（hy3 额度/账号切换）见 memory
  `codebuddy-cli-credential-chain` / `dispatch-target-codebuddy-hy3`。
