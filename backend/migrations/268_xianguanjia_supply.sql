-- 268_xianguanjia_supply.sql
-- 闲管家「虚拟货源提卡」方向：货源凭证两列。
--
-- 背景（派发单 D6a）：D6 重建虚拟货源被调模式，闲管家用「六段签名」调我方
--   /api/v1/xgj-supply/* 接口，验签需要 supply_app_id / supply_app_secret 两个
--   应用侧凭证。mch_id / mch_secret_encrypted 两列在 252 表建表时已预留，本
--   方向正是其对应物（货源授权商户），直接复用，不再追加。
--
-- 存储选型：新增两列而非 settings KV。理由：两列与既有 ERP 凭证同属
--   xianyu_xianguanjia_config「闲管家单行配置」，同表同行可保证一次保存、
--   同一 active 行、admin 一次读写的一致性；settings KV 无结构无类型，且会
--   与业务键混用，不如本表语义贴合。
--
-- up-only 说明（为何不提供 .down.sql）：
--   两列均为「只增不删」的凭证列，默认空串保持既有行兼容，对旧代码无副作用
--   （旧代码不 SELECT 这两列）。Down 迁移需要 DROP COLUMN 会丢凭证，属不可逆
--   的数据损毁；仓库内绝大多数结构性迁移同样 up-only（如 252 本身）。故仅提供
--   up 方向，回滚依赖应用层停用货源模式而非删列。
--
-- 幂等：ADD COLUMN IF NOT EXISTS；仅随代码提交，不自动应用到生产库。

ALTER TABLE xianyu_xianguanjia_config
    ADD COLUMN IF NOT EXISTS supply_app_id VARCHAR(120) NOT NULL DEFAULT '';

ALTER TABLE xianyu_xianguanjia_config
    ADD COLUMN IF NOT EXISTS supply_app_secret_encrypted TEXT NOT NULL DEFAULT '';
