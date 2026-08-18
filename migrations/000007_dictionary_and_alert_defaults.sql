-- 000007_dictionary_and_alert_defaults.sql
-- 发布当前已确认的指标字典版本，并提供可直接运行的合同到期预警默认规则。
INSERT INTO metric_dict_version (version, released_at, checksum)
VALUES ('2026-07-28', UTC_TIMESTAMP(3), NULL)
ON DUPLICATE KEY UPDATE version = VALUES(version);

INSERT INTO alert_rule (id, rule_code, name, source_fct, severity, enabled, threshold_json, created_at, updated_at)
VALUES ('01J00000000000000000000001', 'CONTRACT_EXPIRY', '合同到期提醒', 'dim_contract', 'HIGH', 1, '{"days":30}', UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))
ON DUPLICATE KEY UPDATE name = VALUES(name), source_fct = VALUES(source_fct), severity = VALUES(severity), threshold_json = VALUES(threshold_json), updated_at = VALUES(updated_at);
