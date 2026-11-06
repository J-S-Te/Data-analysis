// Package auth 定义认证边界输出 Principal（对齐 CRM internal/shared/auth/principal.go）。
package auth

import (
	"context"
	"errors"
)

// ErrSessionUnavailable 保留会话依赖故障，供中间件区分确定登出。
var ErrSessionUnavailable = errors.New("session service is temporarily unavailable")

// Principal 只有在平台令牌验签与服务端会话复核后才可写入；业务层不得从客户端字段重建。
type Principal struct {
	UserID                 string
	Username               string
	TenantID               string
	DisplayName            string
	Roles                  []string
	Permissions            map[string]struct{}
	ScopeMode              string
	OrganizationIDs        []string
	TeamIDs                []string
	RoleConfigHash         string
	AuthzRevision          uint64
	AuthorizationCheckedAt int64 // Unix 时间戳。
	ExpiresAt              int64 // Unix 时间戳（绝对期限）。
	// IdleTimeoutSeconds 是会话空闲期限（秒）；IdleRemainingMS 是认证时点剩余可用时长
	// （毫秒，min(空闲截止, 绝对截止) - now，非负）。供 /api/v1/auth/me 展示与前端探测。
	IdleTimeoutSeconds int64
	IdleRemainingMS    int64
}

func (p Principal) HasPermission(permission string) bool {
	_, ok := p.Permissions[permission]
	return ok
}

// HasTenantScope 报告主体是否绑定全租户数据范围。
// 租户级配置写入只允许 ScopeMode == "TENANT"：ORG/SELF 主体只能看各自范围，
// 不能改动影响全租户的字典、预警规则或同步任务。
func (p Principal) HasTenantScope() bool { return p.ScopeMode == "TENANT" }

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
