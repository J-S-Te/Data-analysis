-- 000002_alert_schema.sql
-- 预警中心 Schema（设计方案 §6）：规则 + 快照；阈值参数与平台 cfg 命名空间 data_analysis.alert_rules 同步

CREATE TABLE IF NOT EXISTS alert_rule (
  id              CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  rule_code       VARCHAR(64) NOT NULL,
  name            VARCHAR(128) NOT NULL,
  source_fct      VARCHAR(128) NOT NULL COMMENT '判定基于的聚合表',
  severity        VARCHAR(16)  NOT NULL DEFAULT 'MEDIUM' COMMENT 'LOW/MEDIUM/HIGH',
  enabled         TINYINT(1)   NOT NULL DEFAULT 1,
  threshold_json  JSON         NULL COMMENT '阈值参数（与平台 cfg 同步，变更审计）',
  updated_by      CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
  created_at      DATETIME(3)  NOT NULL,
  updated_at      DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_alert_rule (rule_code)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS alert_item (
  id          CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id   CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  alert_type  VARCHAR(32) NOT NULL COMMENT 'CONTRACT_EXPIRY/PAYMENT_OVERDUE/PROJECT_DELAY/REPORT_OVERDUE/ISSUE_UNCLOSED',
  rule_code   VARCHAR(64) NOT NULL,
  severity    VARCHAR(16) NOT NULL DEFAULT 'MEDIUM',
  target_ref  VARCHAR(128) NOT NULL COMMENT '幂等键：合同/项目/报告/异常 ID',
  title       VARCHAR(255) NOT NULL,
  due_date    DATE NULL,
  status      VARCHAR(16) NOT NULL DEFAULT 'OPEN' COMMENT 'OPEN/ACK/CLOSED',
  closed_at   DATETIME(3) NULL,
  created_at  DATETIME(3) NOT NULL,
  updated_at  DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_alert_target (tenant_id, alert_type, target_ref),
  KEY idx_alert_status (tenant_id, status, severity, due_date)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
COMMENT='alert-worker 每晚幂等 upsert；预警中心按范围过滤读取';
