// Package oidc 实现 OIDC 登录事务、服务端会话与嵌入令牌（对齐 CRM crmauth 模式）。
package oidc

import (
	"encoding/json"
	"time"
)

// LoginTransaction 保存 PKCE verifier 与 nonce（加密），state 单次消费。
type LoginTransaction struct {
	StateHash          string    `gorm:"column:state_hash;primaryKey"`
	TenantID           string    `gorm:"column:tenant_id"`
	NonceCipher        []byte    `gorm:"column:nonce_cipher"`
	CodeVerifierCipher []byte    `gorm:"column:code_verifier_cipher"`
	ReturnPath         string    `gorm:"column:return_path"`
	ExpiresAt          time.Time `gorm:"column:expires_at"`
	CreatedAt          time.Time `gorm:"column:created_at"`
}

func (LoginTransaction) TableName() string { return "da_oidc_login_transactions" }

// Session 是服务端会话（cookie 只存随机 token 的哈希）。
type Session struct {
	SessionIDHash          string     `gorm:"column:session_id_hash;primaryKey"`
	TenantID               string     `gorm:"column:tenant_id"`
	PlatformUserID         string     `gorm:"column:platform_user_id"`
	DisplayName            string     `gorm:"column:display_name"`
	RolesJSON              string     `gorm:"column:roles_json"`
	PermissionsJSON        string     `gorm:"column:permissions_json"`
	RoleConfigHash         string     `gorm:"column:role_config_hash"`
	AuthzRevision          uint64     `gorm:"column:authz_revision"`
	AccessTokenCipher      []byte     `gorm:"column:access_token_cipher"`
	ExpiresAt              time.Time  `gorm:"column:expires_at"`
	AuthorizationCheckedAt time.Time  `gorm:"column:authorization_checked_at"`
	CreatedAt              time.Time  `gorm:"column:created_at"`
	LastSeenAt             time.Time  `gorm:"column:last_seen_at"`
	RevokedAt              *time.Time `gorm:"column:revoked_at"`
}

func (Session) TableName() string { return "da_oidc_sessions" }

// EmbedToken 嵌入令牌：短 TTL、单次消费（设计方案 §4.4）。
type EmbedToken struct {
	TokenHash      string     `gorm:"column:token_hash;primaryKey"`
	TenantID       string     `gorm:"column:tenant_id"`
	PlatformUserID string     `gorm:"column:platform_user_id"`
	DashboardCode  string     `gorm:"column:dashboard_code"`
	ScopeJSON      string     `gorm:"column:scope_json"`
	ExpiresAt      time.Time  `gorm:"column:expires_at"`
	ConsumedAt     *time.Time `gorm:"column:consumed_at"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
}

func (EmbedToken) TableName() string { return "da_embed_tokens" }

func (s *Session) Roles() []string {
	var roles []string
	_ = json.Unmarshal([]byte(s.RolesJSON), &roles)
	return roles
}

func (s *Session) Permissions() map[string]struct{} {
	var raw []string
	_ = json.Unmarshal([]byte(s.PermissionsJSON), &raw)
	result := make(map[string]struct{}, len(raw))
	for _, item := range raw {
		result[item] = struct{}{}
	}
	return result
}

// 注意：不使用 AutoMigrate——表结构一律由版本化 SQL（migrations/000003_oidc_sessions.sql）创建。
// 模型仅供 GORM 读写使用。
