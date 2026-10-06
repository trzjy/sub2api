-- 246_account_credentials_mac_columns.sql
-- 凭证静态加密（E2）配套：accounts 增加两个由 Go 写路径维护的 HMAC 指纹列。
--
-- 背景：credentials 的敏感子键静态加密（enc:v1: 前缀，AES-256-GCM，nonce 随机）后，
-- SQL 语句无法再对密文做"明文相等"判断（同一明文两次加密产出不同密文）。依赖这类
-- 判断的既有逻辑全部改为比对指纹列：
--   - credentials_mac：credentials 规范化明文 JSON 的 HMAC-SHA256（hex）。
--     供 Grok 凭证 CAS 守卫、上游倍率探测锚点、UpdateCredentials 的快照失效判断
--     等"整份凭证是否一致"的比较使用。
--   - credentials_api_key_mac：credentials.api_key 明文的 HMAC-SHA256（hex）。
--     供 Ollama Cloud 用量分组按 api_key 匹配兄弟账号 / 聚合分组活动使用。
--
-- 列由 repository 写路径在每次写 credentials 时同步维护（含加密未启用的部署，
-- 此时指纹照常计算）。存量行的指纹由 E3 存量迁移任务回填；为 NULL 时相关守卫
-- 视为"不匹配"（安全侧失败），不会误放行。

ALTER TABLE accounts ADD COLUMN IF NOT EXISTS credentials_mac text;
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS credentials_api_key_mac text;
