-- 237_xianyu_account_cookie_detail.sql
-- 闲鱼账号投影补充 Cookie 续期明细：cookie_status 改为由 Worker 最近一次自动续期结果推导
-- （valid/invalid/expiring），仅当判定为失效时在 cookie_detail 保存 Worker 返回的失败原因，
-- 供账号列表悬浮提示与 Cookie 失效告警使用。此前 cookie_status 被同步逻辑写死为 unknown，
-- 告警巡检的失效分支永远无法触发。

ALTER TABLE xianyu_accounts
    ADD COLUMN IF NOT EXISTS cookie_detail VARCHAR(500) NOT NULL DEFAULT '';
