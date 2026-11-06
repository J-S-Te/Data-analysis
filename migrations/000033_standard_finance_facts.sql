-- 标准财务事实使用独立命名空间，不覆盖任何现有集成快照和事实。
CREATE TABLE IF NOT EXISTS standard_finance_source (
 tenant_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 source_code VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 version BIGINT NOT NULL DEFAULT 0,
 batch_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
 published_at DATETIME(3) NULL,
 PRIMARY KEY (tenant_id, source_code)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS standard_finance_batch (
 tenant_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 source_code VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 batch_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 version BIGINT NOT NULL,
 schema_version INT NOT NULL,
 digest CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 as_of_date CHAR(10) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 row_count INT NOT NULL,
 actor_id VARCHAR(128) NOT NULL,
 published_at DATETIME(3) NOT NULL,
 snapshot_json JSON NOT NULL,
 PRIMARY KEY (tenant_id, source_code, batch_id),
 UNIQUE KEY uk_standard_finance_version (tenant_id, source_code, version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS standard_finance_fact (
 tenant_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 source_code VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 batch_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 fact_type VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 external_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 payload_json JSON NOT NULL,
 PRIMARY KEY (tenant_id, source_code, batch_id, fact_type, external_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
