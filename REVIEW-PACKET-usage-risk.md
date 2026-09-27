# REVIEW PACKET · 异常调用分析——R12 六项修复增量验证审（第 9 轮·窄口径）

本文件是审查配套材料（非被审代码）。**本轮为窄口径验证审**：第 8 轮（证据 `review-r11/2026-09-25T14-06-19-246Z-review/`）结论 block 6 项（全 P2，范围外旧代码），主会话逐条直读预核采纳 6/拒绝 0（`review-r11/ADJUDICATION-round8.md`），经用户三裁定（同批派卡修 / min_daily_requests=10 追认 / 窄口径外审⑨）后 R12 修复完成。**本轮只裁 R12 六项修复差异及其执行完整性。**

## 0. 审查范围契约（本轮硬边界）

- **只裁**：R12 四卡触碰文件的差异（§1 清单 + §2 映射）+ SSOT 回写执行完整性。
- **不作为发现项**：①R1-R8 各轮已审且 R12 未触碰的代码；②R11 DST 修复（第 8 轮零发现通过）；③方案修订方向（用户最高权威裁定，只可审执行）；④登记在案事项（§4）。
- 旧代码若发现新问题：登记不阻断（P0 级运行时必炸类除外），由主会话呈用户裁定。
- **执行方式披露**：R12-1/R12-2 代理首跑 524 断连后重派成功；R12-3 首发成功；R12-4 两连败（断流+524）转主会话代执行（卡面修法与白名单不变，台账+RESULT 如实登记）；R12-1 另有主会话验收补丁（§3）。此为流程偏离披露项，可挑战执行完整性，不改变代码审查范围。

## 1. 权威与范围

- 方案 SSOT：`docs/abnormal-usage-analysis-plan.md`（Final v7 + 实施期修订；六条用户裁定不变；⑤⑥⑦⑧轮修订历史已登记，⑧含 min_daily_requests=10 用户追认）。
- **本轮 R12 差异文件**：
  - `backend/internal/repository/dashboard_aggregation_repo.go`：`isUsageLogsPartitioned` / `listUsageLogsPartitions` 签名加 `exec sqlExecutor`（:660/:684），事务路径调用点传 exec（:253/:730），非事务调用点传 r.sql（:468）；cleanupUsageLogsOnExecutor 增同连接不变量注释
  - `backend/internal/repository/dashboard_aggregation_group_usage_test.go`：新增 `recordingExecutor` + `TestDashboardAggregationR12_1_ProbeAndListUsePassedExecutor`（执行器归属锁定）；:345 过时注释修正
  - `backend/internal/handler/admin/usage_risk_handler.go`：ListReports 六参数严格校验 400（:82-129：user_id/group_id ParseInt、include_low ParseBool、date YYYY-MM-DD、level 枚举 {critical,high,medium,low}、rule `^R([1-8]|3a|3b)$`）；静默忽略反模式注释
  - `backend/internal/handler/admin/usage_risk_handler_test.go`：非法参数 400×7 子用例（含 svc.listCalls==0）+ 合法边界 + 缺省参数三组测试
  - `backend/internal/service/usage_risk_policy.go`：defaultUsageRiskSettings 注释追认（:143，值/键不动）
  - `backend/internal/service/usage_risk_rules.go`：R5 段收集全部达标 IP 按 (Requests 降序, IP 字典序) 取首（:281-302，与 buildIPEvidence 同序，删 break）；AssembleEvidence 的 OriginalBytes 仅超限设置+裁剪循环含最终字段重序列化（:511-539）
  - `backend/internal/service/usage_risk_rules_test.go`：R5 确定性 ×100 稳定 + TruncatedContract + NearLimit 二分逼近边界三组新测试
  - `backend/internal/repository/usage_risk_repo.go`：uaWhitelistTerms 改名+字面直传（:313-326）；谓词 strpos+COALESCE（:351-354）；两消费点删哨兵（:377/:439）
  - `backend/internal/repository/usage_risk_repo_test.go`：R7 白名单语义锁定升级（strpos/禁 LIKE ANY）+ R12_4 字面直传与空白名单 NULL 锁定
  - `docs/abnormal-usage-analysis-plan.md`：修订历史⑧（六项+三裁定+默认值追认）

## 2. 外审⑧ 6 项 → R12 修复映射（请挑战整改正确性与完整性）

| # | 发现 | 整改 | 锚点 | 锁定证据 |
|---|---|---|---|---|
| 1 | 清理事务路径分区探测/枚举绕 Tx 借池连接（MaxOpenConns=1 互锁） | 探测/枚举签名加 exec；事务路径统一传入（探测+枚举+DROP+汇总同连接）；非事务调用点传 r.sql 行为不变 | repo :253/:660/:684/:730/:468 | recordingExecutor 归属断言（sqlmock db/tx 期望共享队列无法从语句顺序区分——**主会话验收补丁**，执行器原 sqlmock 顺序锁定无效已登记）；mutation（函数内 exec→r.sql）FAIL→还原 identical→PASS |
| 2 | 筛选参数静默忽略（user_id=abc 退化全量；无效日期 500） | 六参数已提供即严格校验失败 400（未提供不进条件）；level/rule 枚举按规则引擎实据（R3a/R3b 放宽有注释） | handler :82-129 | 非法×7 子用例 400 且 svc.listCalls==0；合法边界+缺省行为不变 |
| 3 | min_daily_requests=10 无权威（U2 轮实现取值回写方案的权威倒置） | **用户追认 10**（2026-09-25 AskUserQuestion）：SSOT 修订历史⑧权威链补正（用户裁定→方案 §4→实现）；policy 注释同步（值/键不动） | policy :143 + docs 修订历史⑧ | 文本直读；注释与方案一致 |
| 4 | R5 命中 IP map 遍历抖动（detail 不确定） | 收集全部达标 IP 按 (Requests 降序, IP 字典序) 排序取首——比较器与 buildIPEvidence 同序；命中判定与分数不变 | rules :281-302 | R5Deterministic ×100 次稳定断言（requests 最大/同值字典序首）；mutation（取末项）FAIL |
| 5 | UA 白名单 SQL LIKE 通配语义+空哨兵 vs 规则侧字面匹配（事实分叉） | uaWhitelistTerms 字面直传（滤空串）；谓词 `NOT COALESCE((SELECT bool_or(strpos(ul.user_agent,t)>0) FROM unnest($4::text[]) t), false)`——单一形态参数序不变；空哨兵删除（nil→NULL→unnest 零行→COALESCE false→全部计入，与规则侧 isWhitelistedUA(ua,[])==false 一致）。**形态取舍**：重派裁定原指定方案 A（空时不拼谓词），直读后改方案 B——谓词在 COUNT FILTER 内部，A 需两 SQL 变体，B 单一形态满足"最小改动且参数序正确"核心约束 | repo :313-326/:351-354/:377/:439 | R7 语义锁定升级（Contains strpos / NotContains "LIKE ANY"）；R12_4：terms 字面原文（%/_ 不包装）+空白名单 NULL 直传（WithArgs(..., nil)）；mutation（退回 LIKE ANY）FAIL |
| 6 | evidence 上限校验不含 original_bytes 且未超限也设置 | OriginalBytes 仅超限分支设置；裁剪循环与上限判定统一用含全部最终字段的 json.Marshal(ev)（未超限路径零值 omitempty 使两次 marshal 等价） | rules :511-539 | AssembleEvidence/ContractGolden 未超限无 original_bytes 键；TruncatedContract 超限有标记且最终 ≤256KB；NearLimit 二分构造 [256KB-25, 256KB] 区间断言最终不越限；mutation（无条件设置）三测试 FAIL |

## 3. Pre-closure Review（两项已执行）

### 3.1 Execution Omission Review
外审⑧ 6 项→四卡映射逐条核对（§2 表）；用户三裁定逐条落地（同批派卡/R12 四卡、默认值追认/SSOT+注释、窄口径⑨/本 packet）；锚点全部主会话直读证实；R12 锁定测试全部 -v PASS。

### 3.2 Global Consistency Review（第 12 轮闭环自审，零新增）
- UA 白名单单一路径：uaWhitelistTerms 唯一定义恰两消费点（AggregateHourly/RebuildHourlyWindow INSERT 同一 SQL 派生），规则侧 isWhitelistedUA 同字面口径；旧链残留扫描全零（uaWhitelistPatterns 旧名/LIKE ANY/空哨兵 grep 均 0 命中）。
- 探测/枚举调用点全景：isUsageLogsPartitioned 恰两处（:253 事务 exec / :468 非事务 r.sql），listUsageLogsPartitions 恰一处（:730 传 exec）。
- R5 排序比较器与 buildIPEvidence 代码直读同序；R12-2 的 400 形态复用 response.BadRequest，前端契约无字段变化（400 由既有 axios 错误路径处理）。
- 无资金类路径；无行为语义扩散（R12-1 非事务路径字节级不变、R12-3 值/键不动）。

## 4. 登记在案（不得重复报告）
①DB 实库行为待补验清单（CTE 实库解析/INSERT 等宽/DST 实桶/jsonb contains/**strpos/unnest/COALESCE 空数组 NULL 直传**等文本级盲区，见 99-final-report.md）；②既有失败四项（TestWebDeepseekRegisterSendCodeSuccess/TestOpenAIResponseFlush 全包挂起/CodeBuddyShadowWizard 4 失败/SupportedModelChip 1 失败——HEAD 干净树证明既有，各轮收敛对证同名复现零回归）；③repository 层 dashboard 测试 `-tags unit` 跑道约定；④usage_risk_* 阈值前端编辑 UI 另立小单；⑤容量残留（病态巨日单日预备>总预算跨轮停滞，SSOT §5 已登记）；⑥ent 双管理查明结论；⑦外审⑤ stderr 三疑点（其中 user_id 静默忽略已随 R12-2 #2 闭环，其余两项仍登记）；⑧ListCandidates UNION 窗口扫描 1→3（§6.2 既定代价）；⑨R11 residual risk（半小时 DST 回拨+窗口首小时→潜在重插冲突，老实现同存，SSOT 修订历史⑦登记待用户裁定）；⑩全部改动未提交（提交前须请示用户）。

## 5. 关键不变量（请重点挑战）
1. **事务同连接**：CleanupUsageLogsTx 全路径（探测+枚举+DROP+汇总）在同一传入 Tx 上；非事务路径行为字节级不变。
2. **失败关闭校验**：已提供的筛选参数非法即 400（不静默扩大查询）；未提供不进条件。
3. **UA 字面统一**：SQL strpos 与规则引擎 containsSubstr 字面同口径；配置含 %/_ 原样直传；空白名单=全部计入（两路径一致）。
4. **R5 确定性**：detail IP 选择 (Requests 降序, IP 字典序) 首项；命中判定/分数不变。
5. **evidence 硬上限**：最终落库形态（含 original_bytes/truncated）≤256KB；original_bytes 仅超限出现。
6. **默认值权威**：min_daily_requests=10 用户追认（SSOT 修订历史⑧）；policy 注释与方案一致。
7. **检测器零自动动作/告警仅仪表盘**（用户裁定①②，R12 未触碰）。
8. **失败关闭无降级**：无 fallback/兜底（#5 修法是语义统一而非放宽谓词；#6 是口径修正而非放大上限）。

## 6. 验证证据（2026-09-25，R12 后收敛，`05-convergence-r12.txt`）
- 合并收敛：build OK；vet(repo+service+handler) OK；repo TestUsageRisk 42 PASS/0 FAIL；repo -tags unit ok（15.3s）；handler admin ok；service TestUsageRisk 定向 ok；service 全量失败名单对证（02 登记既有失败同名复现零回归，见收敛文件）。
- mutation 反证：R12-1（探测函数内 exec→r.sql：归属断言 FAIL）；R12-3 A（R5 取末项 FAIL）/B（OriginalBytes 无条件设置：三测试 FAIL）；R12-4（谓词退回 LIKE ANY：R7 断言 FAIL）。全部还原 diff 校验 identical 后复跑 PASS。
- 台账：`.omc/plans/usage-risk-cards/00-dispatch.md` R12 段（四波故障+代执行+验收补丁+第 12 轮闭环零新增）；R12-1/2/3/4 RESULT。
- 证据目录：`/home/zjy/.sub2api-acceptance/usage-analysis-impl-W5-2026-09-25/`。
