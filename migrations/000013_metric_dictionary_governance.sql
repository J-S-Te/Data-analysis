-- Tenant metric governance. Definitions are metadata, never executable SQL.
SET @metric_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'metric_definition' AND column_name = 'enabled'), 'SELECT 1', 'ALTER TABLE metric_definition ADD COLUMN enabled TINYINT(1) NOT NULL DEFAULT 0');
PREPARE metric_stmt FROM @metric_ddl;
EXECUTE metric_stmt;
DEALLOCATE PREPARE metric_stmt;
SET @metric_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'metric_definition' AND column_name = 'origin'), 'SELECT 1', 'ALTER TABLE metric_definition ADD COLUMN origin VARCHAR(16) NOT NULL DEFAULT ''CUSTOM''');
PREPARE metric_stmt FROM @metric_ddl;
EXECUTE metric_stmt;
DEALLOCATE PREPARE metric_stmt;
SET @metric_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'metric_definition' AND column_name = 'deleted_at'), 'SELECT 1', 'ALTER TABLE metric_definition ADD COLUMN deleted_at DATETIME(3) NULL');
PREPARE metric_stmt FROM @metric_ddl;
EXECUTE metric_stmt;
DEALLOCATE PREPARE metric_stmt;

-- Preserve builtin identity for legacy tenant overrides.
UPDATE metric_definition SET origin = 'BUILTIN' WHERE code IN
 ('1.1','1.2','1.3','1.4','1.5','2.1','2.2','2.3','2.4','2.5','2.6','3.1','3.2','3.3','3.4','4.1','4.2','4.3','4.4','4.5','4.6','4.7');

CREATE TABLE IF NOT EXISTS metric_definition_version (
 id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 metric_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 version BIGINT NOT NULL,
 metric_snapshot JSON NOT NULL,
 operation VARCHAR(32) NOT NULL,
 actor_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
 created_at DATETIME(3) NOT NULL,
 PRIMARY KEY (id),
 UNIQUE KEY uk_metric_version (tenant_id, metric_id, version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS metric_definition_reference (
 id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 metric_code VARCHAR(64) NOT NULL,
 reference_type VARCHAR(64) NOT NULL,
 reference_id VARCHAR(128) NOT NULL,
 PRIMARY KEY (id),
 UNIQUE KEY uk_metric_reference (tenant_id, metric_code, reference_type, reference_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
