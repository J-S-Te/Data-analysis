-- 事务内业务审计 Outbox（AUD-IP-2026 / 批次 2）。
CREATE TABLE IF NOT EXISTS bi_audit_outbox (
  id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  event_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  actor_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL,
  action VARCHAR(160) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  resource_type VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  resource_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  login_ip VARBINARY(16) NULL,
  client_ip VARBINARY(16) NULL,
  ip_source VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'UNKNOWN',
  result VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'SUCCESS',
  delivery_status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'pending',
  attempts INT UNSIGNED NOT NULL DEFAULT 0,
  next_attempt_at DATETIME(3) NOT NULL,
  locked_at DATETIME(3) NULL,
  delivered_at DATETIME(3) NULL,
  last_error VARCHAR(1000) NULL,
  created_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_bi_audit_outbox_event (tenant_id, event_id),
  KEY idx_bi_audit_outbox_due (delivery_status, next_attempt_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
