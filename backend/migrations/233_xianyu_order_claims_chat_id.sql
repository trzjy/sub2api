-- 233_xianyu_order_claims_chat_id.sql
-- 新增闲鱼发货会话 ID：Worker 补发需要买家会话，不能复用 buyer_id。

ALTER TABLE xianyu_order_claims
    ADD COLUMN IF NOT EXISTS chat_id VARCHAR(120) NOT NULL DEFAULT '';
