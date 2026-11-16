-- 000040_alert_rule_notification.sql
-- 预警规则通知配置：通知对象、静默时段、升级。
ALTER TABLE alert_rule
  ADD COLUMN notification_json JSON NULL COMMENT '通知配置：recipients/quiet/escalation';
