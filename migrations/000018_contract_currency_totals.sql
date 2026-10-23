-- Preserve the contract source's per-currency totals in the BI snapshot.
-- total_amount_minor remains for backward compatibility but must not be used
-- to add different currencies together.
ALTER TABLE api_contract_dashboard
  ADD COLUMN currency_totals_json JSON NULL AFTER total_amount_minor;
