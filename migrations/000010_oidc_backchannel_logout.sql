CREATE TABLE IF NOT EXISTS da_oidc_backchannel_logout_replay (
  jti_hash VARCHAR(64) NOT NULL PRIMARY KEY,
  expires_at DATETIME(3) NOT NULL,
  created_at DATETIME(3) NOT NULL,
  KEY idx_da_oidc_logout_replay_expiry (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
