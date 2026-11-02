-- 000026_import_report_lifecycle.sql
-- 导入报告发布生命周期修复（BIZ-DATA-04/05/06）：
-- 1) 逐项应用凭据新增撤回时间、重发布次数和上一版摘要，使上游撤回/删除、同版本内容变化
--    和修订回退都可被审计地重放纠正，而不是让整轮发布永久失败；
-- 2) 撤回只标记 WITHDRAWN 并保留历史行，重放重新出现时恢复 APPLIED；
-- 3) 运行表结构不变：last_success_at/published_* 只由完全应用的轮次推进，失败收尾
--    不再覆盖已验证的成功证据。
-- DDL 中断但元数据未登记时，按列存在性重入；不删除、不重写任何既有证明。

SET @import_status_withdrawn_ddl = IF(
 (SELECT COUNT(*) FROM information_schema.columns
  WHERE table_schema=DATABASE() AND table_name='contract_import_report_status'
   AND column_name='withdrawn_at')=0,
 'ALTER TABLE contract_import_report_status ADD COLUMN withdrawn_at DATETIME(6) NULL AFTER error_code',
 'SELECT 1'
);
PREPARE import_status_withdrawn_stmt FROM @import_status_withdrawn_ddl;
EXECUTE import_status_withdrawn_stmt;
DEALLOCATE PREPARE import_status_withdrawn_stmt;

SET @import_status_supersede_ddl = IF(
 (SELECT COUNT(*) FROM information_schema.columns
  WHERE table_schema=DATABASE() AND table_name='contract_import_report_status'
   AND column_name='supersede_count')=0,
 'ALTER TABLE contract_import_report_status ADD COLUMN supersede_count BIGINT UNSIGNED NOT NULL DEFAULT 0 AFTER withdrawn_at',
 'SELECT 1'
);
PREPARE import_status_supersede_stmt FROM @import_status_supersede_ddl;
EXECUTE import_status_supersede_stmt;
DEALLOCATE PREPARE import_status_supersede_stmt;

SET @import_status_prior_digest_ddl = IF(
 (SELECT COUNT(*) FROM information_schema.columns
  WHERE table_schema=DATABASE() AND table_name='contract_import_report_status'
   AND column_name='prior_payload_digest')=0,
 'ALTER TABLE contract_import_report_status ADD COLUMN prior_payload_digest CHAR(64) NOT NULL DEFAULT '''' AFTER supersede_count',
 'SELECT 1'
);
PREPARE import_status_prior_digest_stmt FROM @import_status_prior_digest_ddl;
EXECUTE import_status_prior_digest_stmt;
DEALLOCATE PREPARE import_status_prior_digest_stmt;

-- 撤回/替代后的水位不再作为当前可用项参与读取过滤。
SET @import_status_scope_ddl = IF(
 (SELECT COUNT(*) FROM information_schema.statistics
  WHERE table_schema=DATABASE() AND table_name='contract_import_report_status'
   AND index_name='idx_import_status_item')=0,
 'ALTER TABLE contract_import_report_status ADD KEY idx_import_status_item (tenant_id,item_id,state)',
 'SELECT 1'
);
PREPARE import_status_scope_stmt FROM @import_status_scope_ddl;
EXECUTE import_status_scope_stmt;
DEALLOCATE PREPARE import_status_scope_stmt;
