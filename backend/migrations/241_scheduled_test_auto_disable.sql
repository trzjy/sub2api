-- 241_scheduled_test_auto_disable.sql
-- 定时测试连续失败自动暂停调度：
-- consecutive_failures 记录连续失败次数（成功清零）；auto_disable_threshold 为
-- 触发暂停调度（temp-unschedulable 至下次定时测试）的阈值，0 表示关闭该功能。

ALTER TABLE scheduled_test_plans
    ADD COLUMN IF NOT EXISTS consecutive_failures INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS auto_disable_threshold INT NOT NULL DEFAULT 0;
