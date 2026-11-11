-- 000036_dimension_sync_keys.sql
-- 完整只读快照成功后用于租户级硬删除对账；不涉及历史合同发布凭据。
CREATE TABLE IF NOT EXISTS dimension_sync_key (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  tenant_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  run_id CHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  dimension VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  source_key VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  parent_key VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
  created_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_dimension_sync_key (tenant_id, run_id, dimension, source_key, parent_key),
  KEY idx_dimension_sync_key_gc (tenant_id, dimension, created_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
