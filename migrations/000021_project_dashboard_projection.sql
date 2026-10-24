-- A full trusted stream is the only authority for projection completeness.
-- Existing dim_project rows are preserved without inventing a success marker.
ALTER TABLE dim_project
  MODIFY COLUMN project_id VARCHAR(32) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
  MODIFY COLUMN created_at DATETIME(6) NOT NULL,
  MODIFY COLUMN updated_at DATETIME(6) NOT NULL,
  ADD COLUMN customer_id VARCHAR(64) NOT NULL DEFAULT '',
  ADD COLUMN contract_id VARCHAR(64) NOT NULL DEFAULT '',
  ADD COLUMN snapshot_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '',
  ADD COLUMN risk TINYINT(1) NULL,
  ADD COLUMN in_flight TINYINT(1) NULL,
  ADD COLUMN service_items INT UNSIGNED NULL,
  ADD COLUMN source_updated_at DATETIME(6) NULL;

CREATE TABLE project_projection_state (
  tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  projection_code VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  run_id VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
  generation BIGINT UNSIGNED NOT NULL DEFAULT 0,
  status VARCHAR(16) NOT NULL DEFAULT 'UNKNOWN',
  last_error VARCHAR(64) NOT NULL DEFAULT '',
  started_at DATETIME(6) NULL,
  finished_at DATETIME(6) NULL,
  last_success_at DATETIME(6) NULL,
  published_run_id VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
  published_snapshot_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '',
  published_source_snapshot_at DATETIME(6) NULL,
  published_source_updated_at DATETIME(6) NULL,
  published_total INT UNSIGNED NOT NULL DEFAULT 0,
  published_checksum_sha256 CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
  PRIMARY KEY (tenant_id, projection_code)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE project_projection_stage (
  tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  run_id VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  project_id VARCHAR(32) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
  snapshot_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
  name VARCHAR(255) NOT NULL,
  customer VARCHAR(255) NOT NULL,
  customer_id VARCHAR(64) NOT NULL,
  contract VARCHAR(64) NOT NULL,
  contract_id VARCHAR(64) NOT NULL,
  category VARCHAR(255) NOT NULL,
  team VARCHAR(128) NOT NULL,
  manager VARCHAR(128) NOT NULL,
  manager_identity_id VARCHAR(128) NOT NULL,
  owner_org_id VARCHAR(64) NOT NULL,
  owner_identity_id VARCHAR(128) NOT NULL,
  status VARCHAR(32) NOT NULL,
  progress INT NOT NULL,
  risk TINYINT(1) NOT NULL,
  in_flight TINYINT(1) NOT NULL,
  service_items INT UNSIGNED NOT NULL,
  created_at DATETIME(6) NOT NULL,
  updated_at DATETIME(6) NOT NULL,
  source_updated_at DATETIME(6) NOT NULL,
  PRIMARY KEY (tenant_id, run_id, project_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
