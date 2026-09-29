-- 265_codebuddy_credit_attempt_version_sequence.sql
--
-- 独占所有者：B4 Card B（唯一条件更新入口）。方案
-- docs/codebuddy-b4-implementation-plan.md §1/§3 Card B；权威
-- docs/codebuddy-cockpit-fusion-plan.md §4.1 addendum。
--
-- 目的：为 CodeBuddy 积分快照的 attempt_version 创建 DB 分配的单调号 sequence。
-- 应用进程在抓取开始时调用 nextval 取得版本号，成功/失败提交阶段均不得再次递增
-- （禁应用本地计数器；sequence 调用失败 = 本次抓取失败关闭）。
--
-- 命名与权限（R7/R8 外审回修，钉死）：
--   * sequence 名：seq_codebuddy_credit_attempt_version
--   * 起始值：1（合法 attempt_version ≥ 1；0 是「尚无事件」哨兵）
--   * owner：迁移执行角色（本迁移在迁移 runner 事务内执行，owner = CURRENT_USER，
--     即生产迁移连接的角色——部署 compose 下为 postgres 超级用户）
--   * GRANT USAGE：予应用运行角色 sub2api（部署 compose 的应用 DB 用户；
--     测试环境该角色不存在时跳过授权，避免 GRANT 失败破坏迁移幂等性）
--
-- 执行语义（与迁移 runner 契约一致，internal/repository/migrations_runner.go）：
--   * 本文件是常规迁移（非 *_notx 后缀），runner 把整份内容在**同一个事务**内执行，
--     任一语句失败即整批回滚 → 原子。
--   * 幂等：CREATE SEQUENCE IF NOT EXISTS + OWNER/CURRENT_USER（无副作用）；
--     GRANT 由 DO 块按角色存在性守卫，重复执行为空操作。
--
-- 影响面：只新增一个 sequence 对象；不触碰 accounts/extra 任何键。

CREATE SEQUENCE IF NOT EXISTS seq_codebuddy_credit_attempt_version
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

-- owner = 迁移执行角色（CURRENT_USER）。重复执行 = 空操作（已是 owner）。
ALTER SEQUENCE seq_codebuddy_credit_attempt_version OWNER TO CURRENT_USER;

-- GRANT USAGE 予应用运行角色 sub2api；角色不存在时跳过（测试环境无该角色）。
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'sub2api') THEN
        GRANT USAGE ON SEQUENCE seq_codebuddy_credit_attempt_version TO sub2api;
    END IF;
END $$;
