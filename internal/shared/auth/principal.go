// Package auth 定义认证边界输出 Principal（对齐 CRM internal/shared/auth/principal.go）。
package auth

import "context"

// Principal 只有在平台令牌验签与服务端会话复核后才可写入；业务层不得从客户端字段重建。
type Principal struct {
	UserID                 string
	Username               string
	TenantID               string
	DisplayName            string
	Roles                  []string
	Permissions            map[string]struct{}
	RoleConfigHash         string
	AuthzRevision          uint64
	AuthorizationCheckedAt int64 // unix
	ExpiresAt              int64 // unix
}

func (p Principal) HasPermission(permission string) bool {
	_, ok := p.Permissions[permission]
	return ok
}

type contextKey string

const principalKey contextKey = "principal"

func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalKey, principal)
}

func FromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalKey).(Principal)
	return principal, ok
}

// Authenticator 隔离 OIDC 与会话实现，中间件只消费最终主体。
type Authenticator interface {
	Authenticate(context.Context, string) (Principal, error)
}
