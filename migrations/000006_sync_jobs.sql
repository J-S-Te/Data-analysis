-- 000006_sync_jobs.sql
-- 管理员手工同步请求队列；实际执行由 aggregation-worker 完成。
CREATE TABLE IF NOT EXISTS sync_job (
  id              CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  source_id       CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  subsystem_code  VARCHAR(64) NOT NULL,
  status          VARCHAR(16) NOT NULL DEFAULT 'QUEUED' COMMENT 'QUEUED/RUNNING/SUCCESS/FAILED',
  requested_at    DATETIME(3) NOT NULL,
  started_at      DATETIME(3) NULL,
  finished_at     DATETIME(3) NULL,
  error_message   TEXT NULL,
  PRIMARY KEY (id),
  KEY idx_sync_job_queue (status, requested_at),
  KEY idx_sync_job_source (source_id, requested_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
