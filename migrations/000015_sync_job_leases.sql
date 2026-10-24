-- Queue ownership is durable; an expiry is meaningful only with a named owner.
-- Do not infer the liveness of pre-upgrade RUNNING jobs. Stop old workers and
-- recover selected tenant/job IDs with sync-job-recover-legacy after migration.
ALTER TABLE sync_job
  ADD COLUMN lease_owner CHAR(32) CHARACTER SET ascii COLLATE ascii_bin NULL,
  ADD COLUMN lease_generation BIGINT UNSIGNED NOT NULL DEFAULT 0,
  ADD COLUMN lease_expires_at DATETIME(3) NULL,
  ADD KEY idx_sync_job_lease_recovery (tenant_id, status, lease_expires_at);

CREATE TABLE sync_job_recovery_audit (
  id CHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  job_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  operator_id VARCHAR(128) NOT NULL,
  evidence_sha256 CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  recovered_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_sync_recovery_job (tenant_id, job_id, recovered_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
