-- Persist the platform authorization-context data scopes so dashboard reads can
-- enforce ORG/TEAM boundaries instead of collapsing every session to TENANT.
ALTER TABLE da_oidc_sessions
  ADD COLUMN data_scopes_json JSON NULL AFTER permissions_json;
