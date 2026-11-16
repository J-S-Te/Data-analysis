-- 完整维表先暂存内容；DDL已完成而未记账时允许安全重入。
SET @history_ddl = IF(
  (SELECT COUNT(*) FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='dimension_sync_key' AND column_name='payload')=0,
  'ALTER TABLE dimension_sync_key ADD COLUMN payload JSON NULL', 'SELECT 1'
);
PREPARE history_stmt FROM @history_ddl;
EXECUTE history_stmt;
DEALLOCATE PREPARE history_stmt;
