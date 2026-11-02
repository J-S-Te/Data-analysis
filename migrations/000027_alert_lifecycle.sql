-- 000027_alert_lifecycle.sql
-- 告警生命周期收敛（BIZ-DATA-07）：记录告警关闭原因，使规则评估可以区分
-- "条件离开窗口后自动收敛"与"人工关闭"。只有前者会在条件重新命中时被自动重开；
-- 人工 ACK/CLOSED 不会被规则评估覆盖。默认不启用收敛（见 ALERT_AUTO_RESOLVE），
-- 因此本迁移只新增列与索引，不改变现网告警行为。
-- DDL 中断但元数据未登记时，按列/索引存在性重入；不重写任何既有状态。

SET @alert_closed_reason_ddl = IF(
 (SELECT COUNT(*) FROM information_schema.columns
  WHERE table_schema=DATABASE() AND table_name='alert_item'
   AND column_name='closed_reason')=0,
 'ALTER TABLE alert_item ADD COLUMN closed_reason VARCHAR(32) NOT NULL DEFAULT '''' AFTER closed_at',
 'SELECT 1'
);
PREPARE alert_closed_reason_stmt FROM @alert_closed_reason_ddl;
EXECUTE alert_closed_reason_stmt;
DEALLOCATE PREPARE alert_closed_reason_stmt;

SET @alert_rule_status_ddl = IF(
 (SELECT COUNT(*) FROM information_schema.statistics
  WHERE table_schema=DATABASE() AND table_name='alert_item'
   AND index_name='idx_alert_rule_status')=0,
 'ALTER TABLE alert_item ADD KEY idx_alert_rule_status (tenant_id,rule_code,status)',
 'SELECT 1'
);
PREPARE alert_rule_status_stmt FROM @alert_rule_status_ddl;
EXECUTE alert_rule_status_stmt;
DEALLOCATE PREPARE alert_rule_status_stmt;
