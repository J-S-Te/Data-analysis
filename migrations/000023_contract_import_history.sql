-- 扩展型迁移：保留普通存量缓存，旧普通缓存不参与历史应用证明。
-- DDL中断但元数据未登记时，按列存在性重入；不重复清理任何缓存。
SET @contract_import_origin_ddl = IF(
 (SELECT COUNT(*) FROM information_schema.columns
  WHERE table_schema=DATABASE() AND table_name='dim_contract'
   AND column_name='origin_kind')=0,
 'ALTER TABLE dim_contract ADD COLUMN origin_kind VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT ''NORMAL''',
 'SELECT 1'
);
PREPARE contract_import_origin_stmt FROM @contract_import_origin_ddl;
EXECUTE contract_import_origin_stmt;
DEALLOCATE PREPARE contract_import_origin_stmt;

CREATE TABLE IF NOT EXISTS contract_import_report_run (
 tenant_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 generation BIGINT UNSIGNED NOT NULL DEFAULT 0,
 last_status VARCHAR(32) NOT NULL DEFAULT 'UNKNOWN',
 last_error VARCHAR(64) NOT NULL DEFAULT '',
 last_run_at DATETIME(6) NULL,
 last_success_at DATETIME(6) NULL,
 published_snapshot_id VARCHAR(64) NOT NULL DEFAULT '',
 published_run_id VARCHAR(64) NOT NULL DEFAULT '',
 PRIMARY KEY (tenant_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS contract_import_report_status (
 tenant_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 item_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 source_revision BIGINT UNSIGNED NOT NULL,
 report_release_version BIGINT UNSIGNED NOT NULL,
 required_channels VARCHAR(32) NOT NULL,
 state VARCHAR(32) NOT NULL,
 source_snapshot_id CHAR(64) NOT NULL,
 aggregation_run_id VARCHAR(64) NOT NULL,
 applied_at DATETIME(6) NULL,
 error_code VARCHAR(64) NOT NULL DEFAULT '',
 payload_digest CHAR(64) NOT NULL,
 api_applied BOOLEAN NOT NULL DEFAULT FALSE,
 source_facts_applied BOOLEAN NOT NULL DEFAULT FALSE,
 PRIMARY KEY (tenant_id,item_id,source_revision,report_release_version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS contract_import_history (
 tenant_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 item_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 source_revision BIGINT UNSIGNED NOT NULL,
 report_release_version BIGINT UNSIGNED NOT NULL,
 contract_id VARCHAR(64) NOT NULL,
 import_mode VARCHAR(32) NOT NULL,
 report_mode VARCHAR(32) NOT NULL,
 status VARCHAR(32) NOT NULL,
 start_date DATE NULL,
 end_date DATE NULL,
 owner_user_id VARCHAR(64) NOT NULL,
 owner_org_id VARCHAR(64) NOT NULL,
 amount_minor BIGINT NOT NULL,
 currency CHAR(3) NOT NULL,
 payload_json JSON NOT NULL,
 source_snapshot_id CHAR(64) NOT NULL,
 aggregation_run_id VARCHAR(64) NOT NULL,
 source_facts_applied BOOLEAN NOT NULL DEFAULT FALSE,
 PRIMARY KEY (tenant_id,item_id),
 KEY idx_import_history_scope (tenant_id,owner_org_id,owner_user_id,start_date,currency)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
