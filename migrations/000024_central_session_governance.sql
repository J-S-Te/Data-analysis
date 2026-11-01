ALTER TABLE da_oidc_login_transactions
    ADD COLUMN central_ticket_cipher VARBINARY(1024) NULL;
ALTER TABLE da_oidc_sessions
    ADD COLUMN refresh_token_cipher BLOB NULL,
    ADD COLUMN access_token_expires_at DATETIME(3) NULL,
    ADD COLUMN central_binding_cipher VARBINARY(1024) NULL,
    ADD COLUMN central_session_id VARCHAR(64) NULL,
    ADD COLUMN oidc_subject VARCHAR(256) NULL,
    ADD COLUMN oidc_sid VARCHAR(256) NULL,
    ADD KEY idx_da_oidc_sid (tenant_id,oidc_sid);
