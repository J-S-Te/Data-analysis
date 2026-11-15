-- 000039_alert_rule_scope.sql
-- 合同到期规则增加可选适用范围与生效时间；空值表示不限制。
ALTER TABLE alert_rule
  ADD COLUMN scope_json JSON NULL COMMENT '合同到期范围：owner_org_ids/customer_types/amount_min/max',
  ADD COLUMN effective_from DATE NULL COMMENT '规则生效起始日（含）',
  ADD COLUMN effective_to DATE NULL COMMENT '规则生效截止日（含）';
