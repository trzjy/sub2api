-- 251_xianyu_exposure_facts.sql
-- 闲鱼曝光助手：商品事实档案列。
--
-- 事实档案是 AI 从商品标题+描述正文中严格提取的 JSON（只含明确陈述的事实，
-- 未提及的维度为 null），作为标题生成唯一可用的事实来源，防止 AI 脑补卖点。
-- 档案随商品描述变化重建（detail hash 比对），管理端可查看/手动修正。

ALTER TABLE xianyu_products
    ADD COLUMN IF NOT EXISTS exposure_facts TEXT,
    ADD COLUMN IF NOT EXISTS exposure_facts_updated_at TIMESTAMPTZ;
