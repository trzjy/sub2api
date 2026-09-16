-- 253_currency_unification.sql
-- 货币计费与展示统一整改（docs/currency-billing-unification-plan.md §4）。
--
-- 设计决定（SSOT 摘要）：
--   D1 记账货币 = USD（余额/用量/兑换/限额/定价不变）。
--   D2 新设置 key FX_RATES：JSON 对象，语义 1 USD = X <币种>；USD 隐含 1.0 不写入，
--      CNY 必填；替代并删除 SUBSCRIPTION_USD_TO_CNY_RATE。
--   D3 BALANCE_RECHARGE_MULTIPLIER → RECHARGE_MARKUP，语义改为"汇率平价之上的加价系数"
--      （到账 USD = ToUSD(实付, 币种) × markup）。2026-09-15 评审裁定：按原值迁移倍率、
--      不做存量到账保全（当前无充值用户，存量 CNY 1:1 入账语义随本次修正为汇率平价）。
--   D4 plan.price 一律按 USD 解释；plan.currency 收敛为固定 'USD'。
--   D5 payment_orders 新增 currency 列并按快照回填。
--
-- 迁移内所有换算使用同一个 fx_cny，取值优先级：
--   ① 旧 SUBSCRIPTION_USD_TO_CNY_RATE > 0 → ② 旧 FX_RATES.CNY（若已存在）→ ③ 7.15。
--
-- ⚠️ 部署 Runbook 前置：若站点实际定价汇率 ≠ 7.15 且未配置过订阅汇率，必须在升级前
--    把 SUBSCRIPTION_USD_TO_CNY_RATE 设为真实值（它将作为本迁移的 fx_cny 并随后删除）。
-- ⚠️ 数据回滚不承诺无损（price 除法取整有厘级损耗）；回滚以迁移前 pg_dump 备份为唯一手段，
--    配套 down 脚本仅做结构回滚（见 253_currency_unification.down.sql）。

-- ============ 1. FX 种子 ============
-- settings 表不存在 FX_RATES 时插入默认值（fx_cny 由下方 DO 块计算后写入）。
-- 用 DO 块保证 fx_cny 三级优先级只计算一次并与 plan 换算一致。

DO $$
DECLARE
    v_old_rate     TEXT;
    v_old_fx_rates TEXT;
    v_fx_cny       NUMERIC;
    v_json         JSONB;
BEGIN
    -- ① 旧订阅汇率 > 0 优先
    SELECT value INTO v_old_rate FROM settings WHERE key = 'SUBSCRIPTION_USD_TO_CNY_RATE';
    IF v_old_rate IS NOT NULL AND TRIM(v_old_rate) <> '' AND v_old_rate::numeric > 0 THEN
        v_fx_cny := v_old_rate::numeric;
    ELSE
        -- ② 已存在的 FX_RATES.CNY（FX_RATES 语义即 1 USD = X CNY）
        SELECT value INTO v_old_fx_rates FROM settings WHERE key = 'FX_RATES';
        IF v_old_fx_rates IS NOT NULL AND TRIM(v_old_fx_rates) <> '' THEN
            BEGIN
                v_json := v_old_fx_rates::jsonb;
                IF v_json ? 'CNY' AND (v_json->>'CNY')::numeric > 0 THEN
                    v_fx_cny := (v_json->>'CNY')::numeric;
                END IF;
            EXCEPTION WHEN OTHERS THEN
                v_fx_cny := NULL; -- 损坏 JSON 视为未配置
            END;
        END IF;
        -- ③ 兜底 7.15（与既有前端硬编码一致，避免存量语义跳变）
        IF v_fx_cny IS NULL THEN
            v_fx_cny := 7.15;
        END IF;
    END IF;

    RAISE NOTICE '[migration-253] fx_cny = %', v_fx_cny;

    -- FX_RATES 种子：仅在不存在时插入（不覆盖已有配置）
    INSERT INTO settings (key, value, updated_at)
    VALUES ('FX_RATES', jsonb_build_object('CNY', v_fx_cny)::text, NOW())
    ON CONFLICT (key) DO NOTHING;

    -- ============ 2. 订单币种列 ============
    ALTER TABLE payment_orders
        ADD COLUMN IF NOT EXISTS currency VARCHAR(3) NOT NULL DEFAULT 'CNY';

    COMMENT ON COLUMN payment_orders.currency IS
        '订单支付币种（ISO 4217 三字码）。新单由下单快照写入；存量单回填 COALESCE(NULLIF(provider_snapshot->>''currency'',''),''CNY'')。';

    -- 回填：快照有币种用快照（如 stripe 实例 HKD/USD），否则 CN 渠道默认 CNY。
    -- DEFAULT 'CNY' 已覆盖无快照的存量单，此处只处理快照含有效币种的行。
    UPDATE payment_orders
    SET currency = COALESCE(NULLIF(provider_snapshot->>'currency', ''), 'CNY')
    WHERE provider_snapshot IS NOT NULL
      AND NULLIF(provider_snapshot->>'currency', '') IS NOT NULL
      AND currency <> COALESCE(NULLIF(provider_snapshot->>'currency', ''), 'CNY');

    -- ============ 3. plan 价格 USD 化 ============
    -- 幂等标记用 currency 列自身：迁移后全部为 'USD'，重跑时不再进入换算分支。
    IF v_old_rate IS NOT NULL AND TRIM(v_old_rate) <> '' AND v_old_rate::numeric > 0 THEN
        -- 分支 A（旧订阅汇率 > 0）：price 本就按 USD 解释 → 仅改 currency 标记，数值不动。
        UPDATE subscription_plans SET currency = 'USD' WHERE COALESCE(currency, '') <> 'USD';
    ELSE
        -- 分支 B（旧汇率 = 0 / 未配置）：price 按 CNY 直付
        --   → price 与 original_price 同批除以 fx_cny 后 ROUND(...,2)（NULL 不动），再标记 USD。
        -- ⚠️ 幂等标记 currency <> 'USD' 会跳过"旧语义 CNY 直付但标签已手填 'USD'"的存量套餐
        --   （迁移后按 USD 解释，CNY 通道实付 ×fx_cny 跳价）；升级前须按 Runbook §8 步骤 2 预检并先清标签。
        UPDATE subscription_plans
        SET price = ROUND(price / v_fx_cny, 2),
            original_price = CASE
                WHEN original_price IS NOT NULL THEN ROUND(original_price / v_fx_cny, 2)
                ELSE original_price
            END,
            currency = 'USD'
        WHERE COALESCE(currency, '') <> 'USD';
    END IF;

    -- ============ 4. 倍率迁移（原值迁移，不乘汇率）============
    INSERT INTO settings (key, value, updated_at)
    SELECT 'RECHARGE_MARKUP', value, NOW()
    FROM settings WHERE key = 'BALANCE_RECHARGE_MULTIPLIER'
    ON CONFLICT (key) DO NOTHING;

    -- 删除旧 key（含订阅汇率 key：其语义已并入 FX_RATES）
    DELETE FROM settings WHERE key IN (
        'BALANCE_RECHARGE_MULTIPLIER',
        'SUBSCRIPTION_USD_TO_CNY_RATE'
    );
END $$;
