-- Migration: 257_usage_risk_analysis
-- 异常调用分析（防中转商薅羊毛）新增三张离线分析表：
--   1) user_usage_metrics_rollup  —— user+group × 小时桶指标（UTC instant）
--   2) usage_risk_reports         —— user+group × 日风险报告（应用时区日界）
--   3) usage_risk_runs            —— 分析运行覆盖台账
-- 全部幂等（IF NOT EXISTS / IF NOT EXISTS 索引），不触碰任何已有表。
-- 字段权威来源：docs/abnormal-usage-analysis-plan.md §6.1 / §6.2 / §6.3。

-- 1) 小时桶指标聚合表（代理主键 id + 唯一键 (user_id, group_id, bucket_hour)）
CREATE TABLE IF NOT EXISTS user_usage_metrics_rollup (
    id                      BIGSERIAL    PRIMARY KEY,
    user_id                 BIGINT       NOT NULL,
    group_id                BIGINT       NOT NULL,
    bucket_hour             TIMESTAMPTZ  NOT NULL,
    request_count           INT          NOT NULL DEFAULT 0,
    input_tokens_sum        BIGINT       NOT NULL DEFAULT 0,
    output_tokens_sum       BIGINT       NOT NULL DEFAULT 0,
    cache_read_tokens_sum   BIGINT       NOT NULL DEFAULT 0,
    cost_usd_sum            NUMERIC(20,10) NOT NULL DEFAULT 0,
    occupied_ms_sum         BIGINT       NOT NULL DEFAULT 0,
    non_whitelisted_ua_count INT         NOT NULL DEFAULT 0,
    computed_at             TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT user_usage_metrics_rollup_unique UNIQUE (user_id, group_id, bucket_hour)
);

-- 清理用索引：按 bucket_hour 批量物理删
CREATE INDEX IF NOT EXISTS idx_user_usage_metrics_rollup_bucket
    ON user_usage_metrics_rollup (bucket_hour);

-- 2) 日风险报告表（report_id 主键 identity；仅人工流转 status + 仅系统写入 invalidated_at）
CREATE TABLE IF NOT EXISTS usage_risk_reports (
    report_id           BIGSERIAL    PRIMARY KEY,
    user_id             BIGINT       NOT NULL,
    group_id            BIGINT       NOT NULL,
    report_date         DATE         NOT NULL,
    policy_version      BIGINT       NOT NULL DEFAULT 0,
    score               INT          NOT NULL DEFAULT 0,
    level               VARCHAR(20)  NOT NULL,
    rule_hits           JSONB        NOT NULL DEFAULT '[]'::jsonb,
    evidence            JSONB        NOT NULL DEFAULT '{}'::jsonb,
    status              VARCHAR(20)  NOT NULL DEFAULT 'open',
    invalidated_at      TIMESTAMPTZ  NULL,
    status_updated_by   BIGINT       NULL,
    status_updated_at   TIMESTAMPTZ  NULL,
    CONSTRAINT usage_risk_reports_user_group_date_unique UNIQUE (user_id, group_id, report_date),
    CONSTRAINT usage_risk_reports_level_check
        CHECK (level IN ('low', 'medium', 'high', 'critical')),
    CONSTRAINT usage_risk_reports_status_check
        CHECK (status IN ('open', 'acknowledged', 'dismissed', 'resolved'))
);

-- 列表排序（有效报告按日期/分数降序）
CREATE INDEX IF NOT EXISTS idx_usage_risk_reports_date_score
    ON usage_risk_reports (report_date DESC, score DESC)
    WHERE invalidated_at IS NULL;

-- 按用户维度查询
CREATE INDEX IF NOT EXISTS idx_usage_risk_reports_user_date
    ON usage_risk_reports (user_id, report_date);

-- Dashboard 聚合（仅 open 且未失效）
CREATE INDEX IF NOT EXISTS idx_usage_risk_reports_status_open
    ON usage_risk_reports (status, invalidated_at, score)
    WHERE status = 'open' AND invalidated_at IS NULL;

-- 3) 分析运行覆盖台账（run_id 主键 identity；run_at 唯一）
CREATE TABLE IF NOT EXISTS usage_risk_runs (
    run_id                  BIGSERIAL    PRIMARY KEY,
    run_at                  TIMESTAMPTZ  NOT NULL,
    window_start            TIMESTAMPTZ  NOT NULL,
    window_end              TIMESTAMPTZ  NOT NULL,
    candidates_total        INT          NOT NULL DEFAULT 0,
    batches_done            INT          NOT NULL DEFAULT 0,
    batches_failed          INT          NOT NULL DEFAULT 0,
    failed_batches          JSONB        NOT NULL DEFAULT '[]'::jsonb,
    budget_exhausted        BOOLEAN      NOT NULL DEFAULT FALSE,
    recon_cursor_date       DATE         NULL,
    recon_batch_offset      INT          NULL DEFAULT 0,
    recon_batches_done      INT          NOT NULL DEFAULT 0,
    consecutive_partials    INT          NOT NULL DEFAULT 0,
    policy_version          BIGINT       NOT NULL DEFAULT 0,
    r1_reeval_pending       BOOLEAN      NOT NULL DEFAULT FALSE,
    policy_snapshot         JSONB        NOT NULL DEFAULT '{}'::jsonb,
    history_covered         BOOLEAN      NOT NULL DEFAULT FALSE,
    finished_at             TIMESTAMPTZ  NULL,
    failure_stage           VARCHAR(30)  NULL,
    status                  VARCHAR(20)  NOT NULL DEFAULT 'running',
    CONSTRAINT usage_risk_runs_run_at_unique UNIQUE (run_at),
    CONSTRAINT usage_risk_runs_status_check
        CHECK (status IN ('running', 'completed', 'partial'))
);

-- 状态过滤（新鲜度观测）
CREATE INDEX IF NOT EXISTS idx_usage_risk_runs_status
    ON usage_risk_runs (status);
