-- Only materialized, tenant-scoped aggregates are retained; no report/customer contents.
CREATE TABLE business_dashboard_snapshot (
  tenant_id VARCHAR(64) NOT NULL,
  section VARCHAR(32) NOT NULL,
  snapshot_at DATETIME(3) NOT NULL,
  payload JSON NOT NULL,
  PRIMARY KEY (tenant_id, section)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

ALTER TABLE dim_opportunity
  ADD COLUMN created_at DATETIME(3) NULL,
  ADD COLUMN contract_id VARCHAR(26) NULL;

CREATE TABLE fct_opportunity_conversion (
  tenant_id VARCHAR(64) NOT NULL,
  period_month CHAR(7) NOT NULL,
  opportunity_count BIGINT NOT NULL,
  won_contract_count BIGINT NOT NULL,
  snapshot_at DATETIME(3) NOT NULL,
  PRIMARY KEY (tenant_id, period_month)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
