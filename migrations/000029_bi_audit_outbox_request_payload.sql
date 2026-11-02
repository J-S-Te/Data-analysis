-- 000029_bi_audit_outbox_request_payload.sql
-- 审计 outbox 补齐 request_id / payload 列（AUD-IP-2026）：
-- internal/auditoutbox 的 outboxRow 已声明 request_id 与 payload，但 000016 建表没有
-- 这两列，也没有任何迁移补过它们。后果分两侧：
-- 1) 读取侧 Claim() 生成 SELECT *，缺列不报错、而是把这两列静默读成零值——
--    outbox 里的请求追踪号与本地证据永远读不回来，dispatcher 只能用空 RequestID
--    投递，审计与请求的关联在恢复/重投时丢失；
-- 2) 写入侧一旦按列名 INSERT（auditoutbox.Append 的 map 形式），缺列就是真实 MySQL
--    的 ERROR 1054 Unknown column，整条业务事务（业务变更 + 审计）一起回滚。
-- 因此必须补列，且写入方在空值/未迁移窗口下省略这两列（见 internal/auditoutbox）。
-- 1) request_id 记录发起该业务操作的请求追踪号，与平台审计的 request_id 对齐；
-- 2) payload 只保存本地可判定的审计证据（如精确版本三元组：source_revision /
--    report_release_version / state），不含令牌、凭据或查询串原文。
-- 投递器不转发 payload：平台审计 Event 没有负载字段，payload 仅作本地证据。
-- 只新增可空列，不回填、不改写任何既有事件；写入方对空值一律落 NULL。
-- DDL 中断但元数据未登记时，按列存在性重入；不删除、不重写任何既有证明。

SET @audit_outbox_request_id_ddl = IF(
 (SELECT COUNT(*) FROM information_schema.columns
  WHERE table_schema=DATABASE() AND table_name='bi_audit_outbox'
   AND column_name='request_id')=0,
 'ALTER TABLE bi_audit_outbox ADD COLUMN request_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER resource_id',
 'SELECT 1'
);
PREPARE audit_outbox_request_id_stmt FROM @audit_outbox_request_id_ddl;
EXECUTE audit_outbox_request_id_stmt;
DEALLOCATE PREPARE audit_outbox_request_id_stmt;

SET @audit_outbox_payload_ddl = IF(
 (SELECT COUNT(*) FROM information_schema.columns
  WHERE table_schema=DATABASE() AND table_name='bi_audit_outbox'
   AND column_name='payload')=0,
 'ALTER TABLE bi_audit_outbox ADD COLUMN payload JSON NULL AFTER request_id',
 'SELECT 1'
);
PREPARE audit_outbox_payload_stmt FROM @audit_outbox_payload_ddl;
EXECUTE audit_outbox_payload_stmt;
DEALLOCATE PREPARE audit_outbox_payload_stmt;

-- 回退（down，仅受控人工回滚时执行；已发布迁移默认不可逆，正常发布流程不执行）：
-- 先确认没有依赖这些列的读取方，再逐一删除；顺序与新增相反。
-- ALTER TABLE bi_audit_outbox DROP COLUMN payload;
-- ALTER TABLE bi_audit_outbox DROP COLUMN request_id;
