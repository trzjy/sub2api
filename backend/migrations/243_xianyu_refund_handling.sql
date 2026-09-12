-- 243_xianyu_refund_handling.sql
-- 闲鱼退款成功追回兑换码：
-- 1) xianyu_order_claims 增加退款处置审计列；处置（认领+码快照+作废/追回+终态）由站点在
--    单个数据库事务内原子完成，refund_handled_at 非 NULL 即已处置（幂等去重锚点）；
-- 2) 处置动作由站点内部分辨：delivered→作废(expired)，used→按码类型追回（订阅扣天/余额扣值）。

ALTER TABLE xianyu_order_claims
    ADD COLUMN IF NOT EXISTS refund_action TEXT,
    ADD COLUMN IF NOT EXISTS refund_detail TEXT,
    ADD COLUMN IF NOT EXISTS refund_handled_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_xianyu_order_claims_refund_unhandled
    ON xianyu_order_claims (refund_handled_at)
    WHERE refund_handled_at IS NULL;
