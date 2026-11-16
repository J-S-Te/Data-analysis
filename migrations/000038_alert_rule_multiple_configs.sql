-- 000038_alert_rule_multiple_configs.sql
-- 同一租户允许为同一受支持规则编码保存多份命名配置；是否启用由应用层约束。
-- alert_item 仍按 alert_type+target_ref 幂等，因此同一时刻只允许一条同编码规则启用，
-- 避免同一合同被多条规则重复写入同一预警目标。

ALTER TABLE alert_rule
  DROP INDEX uk_alert_rule_tenant,
  ADD UNIQUE KEY uk_alert_rule_tenant_name (tenant_id, rule_code, name);
