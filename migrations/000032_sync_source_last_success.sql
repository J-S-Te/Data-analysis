-- 000032_sync_source_last_success.sql
-- 同步通道“曾经成功”的持久标记（审批中口径切换门禁）
--
-- 背景：DASHBOARD_APPROVAL_SOURCE=instances 只在 contract_approval 通道首次 SUCCESS
-- 之后才允许启用（否则审批中会从“近似 >0”退化成恒 0）。sync_source 只有
-- last_status/last_run_at，一旦下一轮 FAILED 就丢失“曾经成功”的证据，无法表达
-- “首次成功即可切换”的语义，因此补一个可空时间戳列。
--
-- 只新增可空列，旧镜像不读，向后兼容；沿用 000027 的 information_schema 存在性守卫，
-- DDL 中断但元数据未登记时按列存在性重入，不回填、不重写既有状态。
-- 回滚：回退镜像；列可留（不切换 instances 即无副作用）。

SET @sync_source_last_success_ddl = IF(
 (SELECT COUNT(*) FROM information_schema.columns
  WHERE table_schema=DATABASE() AND table_name='sync_source'
   AND column_name='last_success_at')=0,
 'ALTER TABLE sync_source ADD COLUMN last_success_at DATETIME(3) NULL AFTER last_status',
 'SELECT 1'
);
PREPARE sync_source_last_success_stmt FROM @sync_source_last_success_ddl;
EXECUTE sync_source_last_success_stmt;
DEALLOCATE PREPARE sync_source_last_success_stmt;

-- 人工回滚（受控执行，需要 DBA 批准）：
-- ALTER TABLE sync_source DROP COLUMN last_success_at;
