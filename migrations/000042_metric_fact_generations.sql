-- 流式协议只切换完整代次；旧 JSON 快照继续兼容。
SET @history_ddl = IF(
  (SELECT COUNT(*) FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='dashboard_metric_snapshot' AND column_name='generation_id')=0,
  'ALTER TABLE dashboard_metric_snapshot ADD COLUMN generation_id VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT ''''', 'SELECT 1'
);
PREPARE history_stmt FROM @history_ddl;
EXECUTE history_stmt;
DEALLOCATE PREPARE history_stmt;

CREATE TABLE IF NOT EXISTS dashboard_metric_generation (
 id VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 tenant_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 source_code VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 state VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 row_count BIGINT NOT NULL DEFAULT 0,
 sha256 VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
 metadata_json JSON NOT NULL,
 created_at DATETIME(3) NOT NULL,
 PRIMARY KEY(id),
 KEY idx_metric_generation_age(tenant_id,created_at),
 CONSTRAINT chk_metric_generation_state CHECK(state IN ('STAGING','READY','PUBLISHED','DELETING','ABANDONED')),
 CONSTRAINT chk_metric_generation_count CHECK(row_count>=0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS dashboard_metric_fact (
 generation_id VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 sequence BIGINT NOT NULL,
 tenant_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 source_code VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 kind VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 record_id VARCHAR(256) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
 subject_id VARCHAR(256) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '',
 period VARCHAR(7) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
 organization_id VARCHAR(256) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '',
 identity_id VARCHAR(256) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '',
 project_id VARCHAR(256) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '',
 organization_name VARCHAR(256) NOT NULL DEFAULT '',
 identity_name VARCHAR(256) NOT NULL DEFAULT '',
 project_name VARCHAR(256) NOT NULL DEFAULT '',
 status VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 currency VARCHAR(3) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
 bucket VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
 reason_code VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL DEFAULT '',
 amount_minor BIGINT NULL, tax_minor BIGINT NULL,
 actual_minutes BIGINT NULL, capacity_minutes BIGINT NULL,
 pending_count BIGINT NULL, unknown_count BIGINT NULL,
 auto_confirmed_minutes BIGINT NOT NULL DEFAULT 0, auto_confirmed_count BIGINT NOT NULL DEFAULT 0,
 payload_json JSON NOT NULL,
 PRIMARY KEY(generation_id,sequence),
 UNIQUE KEY uq_metric_fact_identity(generation_id,kind,record_id),
 KEY idx_metric_fact_period(generation_id,period),
 KEY idx_metric_fact_org(generation_id,organization_id),
 KEY idx_metric_fact_identity(generation_id,identity_id),
 CONSTRAINT chk_metric_fact_sequence CHECK(sequence>0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
