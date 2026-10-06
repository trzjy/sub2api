-- 234_xianyu_attempt_count_normalize.sql
-- 统一 attempt_count 语义：0=初始自动发货（未补发），N>=1=第 N 次补发。
-- 旧实现 Claim INSERT 写死 attempt_count=1、补发再 +1，导致存量值比新语义大 1，
-- 且自动发货回执 attempt=0 无法匹配 attempt_count=1 的记录（回执被静默丢弃，记录永久 pending）。
-- 一次性减 1 归一化，使 attempt=0 的自动发货回执能关闭存量 pending 订单。

UPDATE xianyu_order_claims
   SET attempt_count = GREATEST(attempt_count - 1, 0);
