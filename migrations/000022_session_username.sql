-- Display only; never used as an authorization identity. Existing sessions stay valid.
ALTER TABLE da_oidc_sessions ADD COLUMN username VARCHAR(255) NOT NULL DEFAULT '';
