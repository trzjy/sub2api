-- quota_keys 新增 device_id：cursor 出站鉴权头 x-jg-auth 中 `d` 字段的值（B-10）。
--
-- 与取号链的 deviceID(code) 是**两个不同用途**，严禁混用：
--   · deviceID(code)  → POST /api/refresh 的 `device-id` 请求头（shareai 拉号接口），按激活码派生；
--   · device_id（本列）→ 出站 x-jg-auth 的 `d`，插件 uuidv4() 随机生成后固化，与激活码无关。
--
-- 用户裁定（2026-10-16）：站点随机生成并落库，每个激活码一份，首次需要时生成、之后永久复用。
-- 故本列绝不可由激活码派生（派生方案已因 D8 推翻）。
--
-- 可重入：ADD COLUMN IF NOT EXISTS，重复执行幂等。
-- 可逆：ALTER TABLE quota_keys DROP COLUMN IF EXISTS device_id;（纯 DDL，不触存量数据）。
--
-- 存量行一律为 NULL = 尚未生成；不预填任何值（预填假值会让上游拿到从未真正使用过的 deviceId，
-- 与插件"首次运行生成"的语义不符）。

ALTER TABLE quota_keys
    ADD COLUMN IF NOT EXISTS device_id VARCHAR(8);

-- 还原 SQL：
--   ALTER TABLE quota_keys DROP COLUMN IF EXISTS device_id;