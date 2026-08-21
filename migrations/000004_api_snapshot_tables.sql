-- 000004_api_snapshot_tables.sql
-- 接口同步快照表（合同/项目系统 internal 接口 → 聚合库；设计方案：看板数据经接口获取）
CREATE TABLE IF NOT EXISTS api_contract_dashboard (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  tenant_id VARCHAR(64) NOT NULL,
  snapshot_at DATETIME(3) NOT NULL,
  total_amount_minor BIGINT NOT NULL DEFAULT 0,
  total_contracts INT NOT NULL DEFAULT 0,
  approval_contracts INT NOT NULL DEFAULT 0,
  active_contracts INT NOT NULL DEFAULT 0,
  expired_contracts INT NOT NULL DEFAULT 0,
  source VARCHAR(32) NOT NULL DEFAULT 'contract-api',
  PRIMARY KEY (id),
  KEY idx_api_contract_dashboard_time (tenant_id, snapshot_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
