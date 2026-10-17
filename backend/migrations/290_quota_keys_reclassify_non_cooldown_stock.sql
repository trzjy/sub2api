-- CUR-F9 / R6-1：非冷却存量标记的一次性处置（不重入、不永久排除）
--
-- 背景（实跑复现）：生产存量行形态为
--   last_refresh_error = 'shareai: upstream cooldown, retry later: 每5分钟内仅可刷新1次'
--   + exhausted = false + status = 'active'
-- （旧版冷却分支所写）。新候选谓词（裁定 3）只查 exhausted=false，
-- 这批行会直接重入补池——但其内容按新分类（classifyUpstreamError / 裁定 2）实为过期终态
-- 或未知失败，既不该按冷却恢复、也不该无脑重拉。
--
-- 一次性数据迁移（幂等、可重复执行）：
--   目标集：last_refresh_error 非空 且 exhausted = false 且 status = 'active'
--   ① 纯冷却语义（命中运行时冷却集「仅可刷新 / 限频 / 冷却」任一，不含任何过期/耗尽措辞）→ 改写为
--      标准冷却形态（加 ErrUpstreamCooldown 哨兵前缀 + 补写 last_refresh_at 窗口锚点，active +
--      exhausted=false 不变）；R12-1 修正：旧版"保持原文"会使这批行迁移后无哨兵前缀、直接重入且无
--      5min 冷却窗口保护；
--   ②-a' 过期/耗尽类（含「已过期 / 额度耗尽 / 已耗尽」）→ status = 'expired'
--          （与 markKeyExpiredByUpstream 同口径：只改 status 维度，保留 exhausted / last_refresh_error / last_refresh_at）；
--   ②-b 未知错误类（既非冷却、亦非过期/耗尽）→ exhausted = true
--          （与 casSetExhausted 同口径：置位后被 ListCandidateKeys 的 Exhausted(false) 查询自然排除）。
--
-- R10-2（CUR-F13，2026-10-17）优先级对齐运行时 classifyUpstreamError：
--   运行时口径为「过期/耗尽 优先于 冷却」——同一条 msg 同时命中两类（既含"已过期"又含
--   "每5分钟内仅可刷新1次"）时归过期。旧迁移的 ②-a 前置 `NOT LIKE '%每%分钟内仅可刷新%次%'`
--   会把此类混合行排除出过期支，而 ②-b 又因含冷却文案把其排除出未知支，结果混合行两头不落、
--   残留为候选重入。本修订把两类支的判定顺序对齐运行时：
--     · ②-a' 删除冷却前置条件，混合行按"含过期词"归 expired（与运行时同归过期一致）；
--     · ① 显式只选纯冷却行（含冷却文案 且 不含 过期/耗尽 措辞），把已被 ②-a' 处理过的混合行
--       排除出冷却支——纯冷却行本就不含这些词，语义不变；
--     · ②-b 维持既有排除集（冷却 + 过期/耗尽 全排除）不变。
--   三条 UPDATE 的 status='active' + exhausted=false 前置条件保证幂等：②-a' 落位后 expired 行
--   不再满足、②-b 落位后 exhausted 行不再满足、① 仅刷新 updated_at 不破坏前置。
--
-- R11 / CUR-F14（外审 R11 must_fix，2026-10-17）冷却识别词与运行时对齐：
--   旧迁移 ① 与 ②-b 的冷却判定唯一依赖 `LIKE '%每%分钟内仅可刷新%次%'`。但运行时
--   classifyUpstreamError（cursor_upstream_client.go）的冷却集是「仅可刷新 / 限频 / 冷却」三词
--   （含不含"每…次"句式的「限频」「冷却」文案）。存量行若只含「限频」或「冷却」（无"每…仅可刷新"
--   句式），旧迁移 ②-b 会误判为非冷却、非过期/耗尽的"未知错误" → 标 exhausted=true → 五分钟窗口
--   期满也不回候选，违背裁定 3（冷却行应按窗口自然恢复）。
--   本修订把 ① 与 ②-b 的冷却识别统一改为与运行时同词表：
--     · ① 冷却命中条件：`LIKE '%仅可刷新%' OR LIKE '%限频%' OR LIKE '%冷却%'`；
--     · ②-b 冷却排除集：同步为 `NOT LIKE 仅可刷新 AND NOT LIKE 限频 AND NOT LIKE 冷却`。
--   这样「限频」「冷却」两类存量行在 ① 中被正确识别为冷却形态并保持（active + exhausted=false），
--   不再被 ②-b 误标 exhausted=true。过期/耗尽优先顺序（R10-2）与既有的排除集不变。
--   本文件仍未在任何生产环境应用（untracked 新文件），直接改写无不可变迁移问题。
--
-- R12-1 / CUR-F15（外审 R12 must_fix，2026-10-17）冷却行改写为标准形态：
--   旧版 ①「保持原文不变」使纯冷却存量行迁移后 active + exhausted=false + 无哨兵前缀文本 →
--   isCooldownRefreshError（quota_key_service.go:1513）判为非冷却 → 直接重入候选且无 5min 冷却
--   窗口保护（立即被再次拉号）。本修订把 ① 从"保持原文"改为"改写为标准冷却形态"：
--     · last_refresh_error = 'shareai: upstream cooldown, retry later: ' || 原文（ErrUpstreamCooldown
--       哨兵前缀，与运行时 writeCooldownMarker 落库形态一致）；
--     · last_refresh_at = COALESCE(last_refresh_at, now())（补写窗口锚点，原文未写锚点的行也得到 now()
--       作为冷却窗口起点）；
--     · WHERE 加 `AND last_refresh_error NOT LIKE 'shareai: upstream cooldown%'` 保证幂等（改写后行
--       带前缀，重跑跳过；运行时已写过标准前缀的存量行同样跳过）。
--   ②-a' / ②-b 不变；① 先于 ②-b 执行，改写后冷却行仍含原三词，被 ②-b 冷却排除集自然排除。
--
-- 还原 SQL（如需回滚）：本迁移只改写存量，逻辑上不可逆为"原分类"，但可按需把
--   status='expired' 行退回 active 或把 exhausted=true 行退回 false（视业务需要），
--   不会改变表结构，无 DDL 风险。
--
-- ⚠ 生产铁律：已 applied 的迁移不可变。本文件为新增 290 编号，仅作用于存量清理；截至本修订
--   仍未在任何生产环境应用（untracked 新文件），直接改写无不可变迁移问题。测试库同理（F9 测试
--   每次自建数据跑迁移）。

-- ① 纯冷却行：改写为标准冷却形态（active + exhausted=false 不变，但补写哨兵前缀 + 窗口锚点）。
-- R12-1（CUR-F15，外审 R12 must_fix）：旧版本支"保持原文不变"会使这批行迁移后
--   active + exhausted=false + 无哨兵前缀文本 → 被 isCooldownRefreshError（quota_key_service.go:1513）
--   判为非冷却 → 直接重入候选且无 5min 冷却窗口保护（立即再被拉号）。故改"改写"：
--     · last_refresh_error = ErrUpstreamCooldown.Error() 前缀 || 原文
--       （'shareai: upstream cooldown, retry later: ' + 原文），与运行时 writeCooldownMarker 落库形态一致；
--     · last_refresh_at = COALESCE(last_refresh_at, now())：补写窗口锚点（原文未写锚点的行也得到 now()
--       作为窗口起点），使 ListCandidateKeys 的 5min 冷却窗口判定生效；
--     · updated_at = now()。
-- 幂等：WHERE 加 `AND last_refresh_error NOT LIKE 'shareai: upstream cooldown%'`——改写后行以哨兵前缀
--   开头，重跑时该支跳过已改写行，避免前缀被反复追加。运行时已写过标准前缀（writeCooldownMarker）的
--   存量行同样被此条件跳过（首部即含前缀），保持现状。
-- R11（CUR-F14）：冷却命中条件统一为运行时冷却集「仅可刷新 / 限频 / 冷却」三词（OR），旧迁移仅依赖
-- `LIKE '%每%分钟内仅可刷新%次%'` 会漏掉只含「限频」「冷却」文案的存量行。R10-2：新增排除
-- 「已过期/额度耗尽/已耗尽」——把已被 ②-a' 标 expired 的混合行排除出冷却支；纯冷却行本就不含这些词，
-- 语义不变。该支在 SQL 层显式与过期支互斥，且与运行时"过期优先"顺序一致。
-- 执行顺序要求：① 必须在本文件内先于 ②-b 执行——改写后冷却行首含"cooldown"字样、且仍含原三词
-- （仅可刷新/限频/冷却），②-b 的冷却排除集（NOT LIKE 三词）已足以将其排除，故 ②-b 排除集保持不变。
UPDATE quota_keys
SET last_refresh_error = 'shareai: upstream cooldown, retry later: ' || last_refresh_error,
    last_refresh_at    = COALESCE(last_refresh_at, now()),
    updated_at         = now()
WHERE last_refresh_error IS NOT NULL
  AND last_refresh_error <> ''
  AND exhausted = false
  AND status = 'active'
  AND ( last_refresh_error LIKE '%仅可刷新%'
        OR last_refresh_error LIKE '%限频%'
        OR last_refresh_error LIKE '%冷却%' )
  AND last_refresh_error NOT LIKE '%已过期%'
  AND last_refresh_error NOT LIKE '%额度耗尽%'
  AND last_refresh_error NOT LIKE '%已耗尽%'
  AND last_refresh_error NOT LIKE 'shareai: upstream cooldown%';

-- ②-a'：过期/耗尽类 → 标 expired（与 markKeyExpiredByUpstream 同口径）。
-- R10-2：删除旧版 `NOT LIKE '%每%分钟内仅可刷新%次%'` 冷却前置条件——混合文案行（既含"已过期"
-- 又含冷却文案）与运行时 classifyUpstreamError「过期/耗尽优先」同口径归过期，而非被旧迁移排除在
-- 过期支外、又被 ②-b 排除在未知支外而残留为候选重入。
UPDATE quota_keys
SET status = 'expired',
    updated_at = now()
WHERE last_refresh_error IS NOT NULL
  AND last_refresh_error <> ''
  AND exhausted = false
  AND status = 'active'
  AND (last_refresh_error LIKE '%已过期%'
       OR last_refresh_error LIKE '%额度耗尽%'
       OR last_refresh_error LIKE '%已耗尽%');

-- ②-b：未知错误类 → 标 exhausted=true（与 casSetExhausted 同口径）。
-- R11（CUR-F14）：冷却排除集同步为运行时冷却集（NOT LIKE 仅可刷新 / 限频 / 冷却），旧迁移仅依赖
-- `NOT LIKE '%每%分钟内仅可刷新%次%'` 会让只含「限频」「冷却」文案的存量行漏出此支、被误标 exhausted=true。
-- R10-2：与 ②-a' 互斥，混合行已由 ②-a' 落位；过期/耗尽排除集不变。
UPDATE quota_keys
SET exhausted = true,
    updated_at = now()
WHERE last_refresh_error IS NOT NULL
  AND last_refresh_error <> ''
  AND exhausted = false
  AND status = 'active'
  AND last_refresh_error NOT LIKE '%仅可刷新%'
  AND last_refresh_error NOT LIKE '%限频%'
  AND last_refresh_error NOT LIKE '%冷却%'
  AND last_refresh_error NOT LIKE '%已过期%'
  AND last_refresh_error NOT LIKE '%额度耗尽%'
  AND last_refresh_error NOT LIKE '%已耗尽%';
