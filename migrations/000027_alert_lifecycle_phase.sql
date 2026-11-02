-- 000027_alert_lifecycle_phase.sql
-- 告警生命周期分阶段收敛（BIZ-DATA-07）：在 000026 的"关闭原因"基础上增加连续
-- 命中/未命中计数与复开审计列，使规则评估可以按"确认轮数"收敛，而不是一次未命中
-- 就关闭：
-- 1) miss_streak 记录连续未命中轮数，达到确认轮数（ALERT_CONFIRM_ROUNDS，默认 2）
--    才写 CLOSED + closed_reason='AUTO_CONDITION_CLEARED'；
-- 2) hit_streak/last_hit_at 记录连续命中轮次与最近命中时间，用于区分"持续到期"
--    与新出现的目标；
-- 3) reopen_count 记录规则自动复开次数，人工关闭永不增加；
-- 4) acknowledged_at/acknowledged_by 保留人工确认证据，自动复开不清空。
-- 只新增列与索引，不回填、不改写任何既有告警状态；自动收敛默认仍关闭
-- （ALERT_AUTO_RESOLVE=false），因此本迁移不改变现网告警行为。
-- DDL 中断但元数据未登记时，按列/索引存在性重入；不删除、不重写任何既有证明。

SET @alert_item_miss_streak_ddl = IF(
 (SELECT COUNT(*) FROM information_schema.columns
  WHERE table_schema=DATABASE() AND table_name='alert_item'
   AND column_name='miss_streak')=0,
 'ALTER TABLE alert_item ADD COLUMN miss_streak TINYINT UNSIGNED NOT NULL DEFAULT 0 AFTER closed_reason',
 'SELECT 1'
);
PREPARE alert_item_miss_streak_stmt FROM @alert_item_miss_streak_ddl;
EXECUTE alert_item_miss_streak_stmt;
DEALLOCATE PREPARE alert_item_miss_streak_stmt;

SET @alert_item_hit_streak_ddl = IF(
 (SELECT COUNT(*) FROM information_schema.columns
  WHERE table_schema=DATABASE() AND table_name='alert_item'
   AND column_name='hit_streak')=0,
 'ALTER TABLE alert_item ADD COLUMN hit_streak TINYINT UNSIGNED NOT NULL DEFAULT 0 AFTER miss_streak',
 'SELECT 1'
);
PREPARE alert_item_hit_streak_stmt FROM @alert_item_hit_streak_ddl;
EXECUTE alert_item_hit_streak_stmt;
DEALLOCATE PREPARE alert_item_hit_streak_stmt;

SET @alert_item_last_hit_at_ddl = IF(
 (SELECT COUNT(*) FROM information_schema.columns
  WHERE table_schema=DATABASE() AND table_name='alert_item'
   AND column_name='last_hit_at')=0,
 'ALTER TABLE alert_item ADD COLUMN last_hit_at DATETIME(3) NULL AFTER hit_streak',
 'SELECT 1'
);
PREPARE alert_item_last_hit_at_stmt FROM @alert_item_last_hit_at_ddl;
EXECUTE alert_item_last_hit_at_stmt;
DEALLOCATE PREPARE alert_item_last_hit_at_stmt;

SET @alert_item_reopen_count_ddl = IF(
 (SELECT COUNT(*) FROM information_schema.columns
  WHERE table_schema=DATABASE() AND table_name='alert_item'
   AND column_name='reopen_count')=0,
 'ALTER TABLE alert_item ADD COLUMN reopen_count INT UNSIGNED NOT NULL DEFAULT 0 AFTER last_hit_at',
 'SELECT 1'
);
PREPARE alert_item_reopen_count_stmt FROM @alert_item_reopen_count_ddl;
EXECUTE alert_item_reopen_count_stmt;
DEALLOCATE PREPARE alert_item_reopen_count_stmt;

SET @alert_item_acknowledged_at_ddl = IF(
 (SELECT COUNT(*) FROM information_schema.columns
  WHERE table_schema=DATABASE() AND table_name='alert_item'
   AND column_name='acknowledged_at')=0,
 'ALTER TABLE alert_item ADD COLUMN acknowledged_at DATETIME(3) NULL AFTER reopen_count',
 'SELECT 1'
);
PREPARE alert_item_acknowledged_at_stmt FROM @alert_item_acknowledged_at_ddl;
EXECUTE alert_item_acknowledged_at_stmt;
DEALLOCATE PREPARE alert_item_acknowledged_at_stmt;

SET @alert_item_acknowledged_by_ddl = IF(
 (SELECT COUNT(*) FROM information_schema.columns
  WHERE table_schema=DATABASE() AND table_name='alert_item'
   AND column_name='acknowledged_by')=0,
 'ALTER TABLE alert_item ADD COLUMN acknowledged_by VARCHAR(64) NOT NULL DEFAULT '''' AFTER acknowledged_at',
 'SELECT 1'
);
PREPARE alert_item_acknowledged_by_stmt FROM @alert_item_acknowledged_by_ddl;
EXECUTE alert_item_acknowledged_by_stmt;
DEALLOCATE PREPARE alert_item_acknowledged_by_stmt;

-- 生命周期收敛按 (租户, 规则, 状态, 连续未命中轮数) 扫描，避免全表扫描 alert_item。
SET @alert_rule_state_ddl = IF(
 (SELECT COUNT(*) FROM information_schema.statistics
  WHERE table_schema=DATABASE() AND table_name='alert_item'
   AND index_name='idx_alert_rule_state')=0,
 'ALTER TABLE alert_item ADD KEY idx_alert_rule_state (tenant_id,rule_code,status,miss_streak)',
 'SELECT 1'
);
PREPARE alert_rule_state_stmt FROM @alert_rule_state_ddl;
EXECUTE alert_rule_state_stmt;
DEALLOCATE PREPARE alert_rule_state_stmt;

-- 回退（down，仅受控人工回滚时执行；已发布迁移默认不可逆，正常发布流程不执行）：
-- ALTER TABLE alert_item DROP KEY idx_alert_rule_state;
-- ALTER TABLE alert_item DROP COLUMN acknowledged_by;
-- ALTER TABLE alert_item DROP COLUMN acknowledged_at;
-- ALTER TABLE alert_item DROP COLUMN reopen_count;
-- ALTER TABLE alert_item DROP COLUMN last_hit_at;
-- ALTER TABLE alert_item DROP COLUMN hit_streak;
-- ALTER TABLE alert_item DROP COLUMN miss_streak;
