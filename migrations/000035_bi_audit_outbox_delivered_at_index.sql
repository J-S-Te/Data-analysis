-- 000035_bi_audit_outbox_delivered_at_index.sql
-- 审计 outbox 已投递行清理的覆盖索引（AUD-IP-2026）。
--
-- 背景：internal/auditoutbox 的 PurgeDelivered 按
--   delivery_status = 'delivered' AND delivered_at < ? ORDER BY delivered_at, id LIMIT ?
-- 分批删除过期的已投递行。000016 只建了 (delivery_status, next_attempt_at) 的
-- idx_bi_audit_outbox_due（服务投递领取，不是清理谓词），因此清理只能扫
-- delivery_status='delivered' 的全部历史行再排序：清理默认关闭（保留期为 0），
-- 一旦显式启用，outbox 里累计的 delivered 行会让每轮清理都做全量过滤+文件排序。
--
-- 处理：只新增二级索引，不改列、不回填、不改写任何既有行。索引列顺序与清理谓词
-- 一致——等值列 delivery_status 在前，范围/排序列 delivered_at 在后；主键 id 在
-- InnoDB 二级索引里隐式跟随，ORDER BY delivered_at, id 可借索引顺序避免额外排序。
-- 大表上 ADD KEY 会重建二级索引并短暂占用 DDL 锁，发布窗口按 DBA 约定执行。
--
-- 可重入：DDL 中断但元数据未登记时，按 information_schema.statistics 判重入
-- （与 000029 一致的判重入风格），索引已存在则执行 SELECT 1 而不重复创建。
-- 回退（down，仅受控人工回滚时执行；已发布迁移默认不可逆，正常发布流程不执行）：
-- ALTER TABLE bi_audit_outbox DROP INDEX idx_bi_audit_outbox_delivered_at;

SET @bi_audit_outbox_delivered_at_index_ddl = IF(
  (SELECT COUNT(*) FROM information_schema.statistics
   WHERE table_schema=DATABASE() AND table_name='bi_audit_outbox'
    AND index_name='idx_bi_audit_outbox_delivered_at')=0,
  'ALTER TABLE bi_audit_outbox ADD KEY idx_bi_audit_outbox_delivered_at (delivery_status, delivered_at)',
  'SELECT 1'
);
PREPARE bi_audit_outbox_delivered_at_index_stmt FROM @bi_audit_outbox_delivered_at_index_ddl;
EXECUTE bi_audit_outbox_delivered_at_index_stmt;
DEALLOCATE PREPARE bi_audit_outbox_delivered_at_index_stmt;
