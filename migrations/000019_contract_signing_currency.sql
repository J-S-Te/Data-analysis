-- Keep contract signing facts per currency. The previous fact table filtered
-- CNY and summed cross-currency rows into one trend amount.
ALTER TABLE fct_contract_signing
  ADD COLUMN currency CHAR(3) NOT NULL DEFAULT 'CNY' AFTER sign_amount_minor;
ALTER TABLE fct_contract_signing
  DROP PRIMARY KEY,
  ADD PRIMARY KEY (tenant_id, period_month, region, customer_category, sales_director_id, sales_id, discount_rate_bucket, currency);
