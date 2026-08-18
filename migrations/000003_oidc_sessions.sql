-- 000003_oidc_sessions.sql
-- OIDC 登录事务 + 服务端会话 + 嵌入令牌（对齐 CRM crm_oidc_* 模式；secret 一律加密存储）

CREATE TABLE IF NOT EXISTS da_oidc_login_transactions (
  state_hash VARCHAR(64) NOT NULL,
  tenant_id VARCHAR(64) NOT NULL,
  nonce_cipher VARBINARY(512) NOT NULL,
  code_verifier_cipher VARBINARY(512) NOT NULL,
  return_path VARCHAR(500) NOT NULL,
  expires_at DATETIME(3) NOT NULL,
  created_at DATETIME(3) NOT NULL,
  PRIMARY KEY (state_hash),
  KEY idx_da_oidc_login_expiry (expires_at),
  KEY idx_da_oidc_login_tenant (tenant_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS da_oidc_sessions (
  session_id_hash VARCHAR(64) NOT NULL,
  tenant_id VARCHAR(64) NOT NULL,
  platform_user_id VARCHAR(128) NOT NULL,
  display_name VARCHAR(200) NOT NULL DEFAULT '',
  roles_json JSON NOT NULL,
  permissions_json JSON NOT NULL,
  role_config_hash VARCHAR(128) NOT NULL,
  authz_revision BIGINT UNSIGNED NOT NULL,
  access_token_cipher BLOB NOT NULL,
  expires_at DATETIME(3) NOT NULL,
  authorization_checked_at DATETIME(3) NOT NULL,
  created_at DATETIME(3) NOT NULL,
  last_seen_at DATETIME(3) NOT NULL,
  revoked_at DATETIME(3) NULL,
  PRIMARY KEY (session_id_hash),
  KEY idx_da_oidc_session_tenant_user (tenant_id, platform_user_id),
  KEY idx_da_oidc_session_expiry (expires_at),
  KEY idx_da_oidc_session_revoked (revoked_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 嵌入令牌（设计方案 §4.4：短 TTL、单次消费；Metabase 仅内网经本表代理访问）
CREATE TABLE IF NOT EXISTS da_embed_tokens (
  token_hash VARCHAR(64) NOT NULL,
  tenant_id VARCHAR(64) NOT NULL,
  platform_user_id VARCHAR(128) NOT NULL,
  dashboard_code VARCHAR(64) NOT NULL,
  scope_json JSON NOT NULL,
  expires_at DATETIME(3) NOT NULL,
  consumed_at DATETIME(3) NULL,
  created_at DATETIME(3) NOT NULL,
  PRIMARY KEY (token_hash),
  KEY idx_da_embed_expiry (expires_at),
  KEY idx_da_embed_user (tenant_id, platform_user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
