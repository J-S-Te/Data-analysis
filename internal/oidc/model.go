// Package oidc 实现 OIDC 登录事务、服务端会话与嵌入令牌（对齐 CRM crmauth 模式）。
package oidc

import (
	"encoding/json"
	"time"
)

// LoginTransaction 保存 PKCE verifier 与 nonce（加密），state 单次消费。
type LoginTransaction struct {
	CentralTicketCipher []byte
	StateHash           string    `gorm:"column:state_hash;primaryKey"`
	TenantID            string    `gorm:"column:tenant_id"`
	NonceCipher         []byte    `gorm:"column:nonce_cipher"`
	CodeVerifierCipher  []byte    `gorm:"column:code_verifier_cipher"`
	ReturnPath          string    `gorm:"column:return_path"`
	ExpiresAt           time.Time `gorm:"column:expires_at"`
	CreatedAt           time.Time `gorm:"column:created_at"`
}

func (LoginTransaction) TableName() string { return "da_oidc_login_transactions" }

// Session 是服务端会话（cookie 只存随机 token 的哈希）。
type Session struct {
	RefreshTokenCipher     []byte
	AccessTokenExpiresAt   time.Time
	CentralBindingCipher   []byte
	CentralSessionID       string
	OIDCSubject            string    `gorm:"column:oidc_subject"`
	OIDCSessionID          string    `gorm:"column:oidc_sid"`
	SessionIDHash          string    `gorm:"column:session_id_hash;primaryKey"`
	TenantID               string    `gorm:"column:tenant_id"`
	PlatformUserID         string    `gorm:"column:platform_user_id"`
	Username               string    `gorm:"column:username"`
	DisplayName            string    `gorm:"column:display_name"`
	RolesJSON              string    `gorm:"column:roles_json"`
	PermissionsJSON        string    `gorm:"column:permissions_json"`
	DataScopesJSON         string    `gorm:"column:data_scopes_json"`
	RoleConfigHash         string    `gorm:"column:role_config_hash"`
	AuthzRevision          uint64    `gorm:"column:authz_revision"`
	AccessTokenCipher      []byte    `gorm:"column:access_token_cipher"`
	ExpiresAt              time.Time `gorm:"column:expires_at"`
	AuthorizationCheckedAt time.Time `gorm:"column:authorization_checked_at"`
	CreatedAt              time.Time `gorm:"column:created_at"`
	LastSeenAt             time.Time `gorm:"column:last_seen_at"`
	// LastInteractiveAt 是最近一次浏览器显式活动（鼠标/键盘，经 /auth/activity）时间。
	// 可为 NULL 以兼容 000025 迁移前的存量行；读取时按 created_at 兜底。
	LastInteractiveAt *time.Time `gorm:"column:last_interactive_at"`
	RevokedAt         *time.Time `gorm:"column:revoked_at"`
}

func (Session) TableName() string { return "da_oidc_sessions" }

// LastInteractiveOrCreated 返回会话最近一次显式活动时间。
// 000025 迁移前的存量行（或迁移回填前的写法）last_interactive_at 为 NULL，按 created_at 处理：
// 既不因升级立刻锁死仍在线会话，也不会让旧会话获得超出升级前的空闲窗口。
func (s *Session) LastInteractiveOrCreated() time.Time {
	if s.LastInteractiveAt != nil && !s.LastInteractiveAt.IsZero() {
		return s.LastInteractiveAt.UTC()
	}
	return s.CreatedAt.UTC()
}

// ValidAt 判断 now 时点会话是否仍有效：未撤销、绝对期限未到、空闲期限未到。
// 参数：now 为判定时点，idle 为空闲期限。
// 返回值：任一项到期均返回 false；调用方不得续期或复活已失效会话。
func (s *Session) ValidAt(now time.Time, idle time.Duration) bool {
	if s.RevokedAt != nil || !now.Before(s.ExpiresAt.UTC()) {
		return false
	}
	return now.Before(s.LastInteractiveOrCreated().Add(idle))
}

// IdleRemaining 返回会话剩余可用时长：min(空闲截止, 绝对截止) - now，非负。
func (s *Session) IdleRemaining(now time.Time, idle time.Duration) time.Duration {
	deadline := s.LastInteractiveOrCreated().Add(idle)
	if deadline.After(s.ExpiresAt.UTC()) {
		deadline = s.ExpiresAt.UTC()
	}
	remaining := deadline.Sub(now)
	if remaining < 0 {
		return 0
	}
	return remaining
}

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

func (s *Session) DataScopes() []DataScope {
	var raw []DataScope
	_ = json.Unmarshal([]byte(s.DataScopesJSON), &raw)
	return raw
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
