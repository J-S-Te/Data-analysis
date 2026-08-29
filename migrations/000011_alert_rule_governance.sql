-- 000011_alert_rule_governance.sql
-- 预警规则治理：版本、执行状态、通知对象与历史保护。所有表均按租户隔离。

CREATE TABLE IF NOT EXISTS alert_rule_version (
  id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  rule_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  version BIGINT NOT NULL,
  rule_snapshot JSON NOT NULL,
  changed_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
  change_reason VARCHAR(255) NULL,
  created_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_alert_rule_version (tenant_id, rule_id, version),
  KEY idx_alert_rule_version_tenant_rule (tenant_id, rule_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS alert_rule_execution (
  id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  rule_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  rule_version BIGINT NOT NULL,
  status VARCHAR(16) NOT NULL COMMENT 'RUNNING/SUCCEEDED/FAILED',
  matched_count INT NOT NULL DEFAULT 0,
  error_message VARCHAR(1024) NULL,
  started_at DATETIME(3) NOT NULL,
  finished_at DATETIME(3) NULL,
  PRIMARY KEY (id),
  KEY idx_alert_rule_execution_latest (tenant_id, rule_id, started_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS alert_rule_recipient (
  id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  rule_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  recipient_type VARCHAR(16) NOT NULL COMMENT 'USER/ROLE/POSITION/ORG',
  recipient_ref CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  created_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_alert_rule_recipient (tenant_id, rule_id, recipient_type, recipient_ref),
  KEY idx_alert_rule_recipient_rule (tenant_id, rule_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
