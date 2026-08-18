-- 000005_api_project_dashboard.sql
-- 项目系统 internal/dashboard 快照表；列定义与 aggregation.APISyncRunner 写入字段一致。
CREATE TABLE IF NOT EXISTS api_project_dashboard (
  id                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  tenant_id          VARCHAR(64) NOT NULL,
  snapshot_at        DATETIME(3) NOT NULL,
  project_count      INT NOT NULL DEFAULT 0,
  in_flight_projects INT NOT NULL DEFAULT 0,
  risk_projects      INT NOT NULL DEFAULT 0,
  service_items      INT NOT NULL DEFAULT 0,
  status_counts_json JSON NOT NULL,
  source             VARCHAR(32) NOT NULL DEFAULT 'project-api',
  PRIMARY KEY (id),
  KEY idx_api_project_dashboard_time (tenant_id, snapshot_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
