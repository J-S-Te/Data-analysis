-- 000012_metric_definitions.sql
-- 租户级指标定义，支持管理员新增和修改并保留版本。
CREATE TABLE IF NOT EXISTS metric_definition (
  id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  code VARCHAR(64) NOT NULL,
  name VARCHAR(128) NOT NULL,
  dashboard VARCHAR(128) NOT NULL,
  definition VARCHAR(512) NOT NULL,
  formula VARCHAR(512) NOT NULL,
  source VARCHAR(255) NOT NULL,
  period VARCHAR(64) NOT NULL,
  status VARCHAR(32) NOT NULL DEFAULT '待确认',
  version BIGINT NOT NULL DEFAULT 1,
  updated_by CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
  created_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_metric_definition_tenant_code (tenant_id, code),
  KEY idx_metric_definition_tenant_dashboard (tenant_id, dashboard)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
