-- 将数据源、同步任务和预警规则改为租户作用域。
-- 迁移前的全局记录归入空租户，仅用于保留历史；Worker 会使用 OIDC_TENANT_ID
-- 为实际租户登记新的数据源，规则中心首次访问时创建该租户的默认规则。

ALTER TABLE sync_source
  ADD COLUMN tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' AFTER id,
  DROP INDEX uk_sync_source,
  ADD UNIQUE KEY uk_sync_source_tenant (tenant_id, subsystem_code, db_schema),
  ADD KEY idx_sync_source_tenant (tenant_id, enabled, subsystem_code);

ALTER TABLE sync_job
  ADD COLUMN tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' AFTER id,
  ADD COLUMN active_key VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER status,
  ADD KEY idx_sync_job_tenant_queue (tenant_id, status, requested_at),
  ADD UNIQUE KEY uk_sync_job_active_source (tenant_id, source_id, active_key);

ALTER TABLE alert_rule
  ADD COLUMN tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' AFTER id,
  DROP INDEX uk_alert_rule,
  ADD UNIQUE KEY uk_alert_rule_tenant (tenant_id, rule_code),
  ADD KEY idx_alert_rule_tenant_enabled (tenant_id, enabled, rule_code);
