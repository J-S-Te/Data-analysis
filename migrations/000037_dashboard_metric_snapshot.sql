-- 每个租户/权威来源只保存一份当前完整快照；历史业务证据保留在源系统。
CREATE TABLE IF NOT EXISTS dashboard_metric_snapshot (
  tenant_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  source_code VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  source_snapshot_at DATETIME(3) NOT NULL,
  collected_at DATETIME(3) NOT NULL,
  payload_json JSON NOT NULL,
  PRIMARY KEY (tenant_id, source_code)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
