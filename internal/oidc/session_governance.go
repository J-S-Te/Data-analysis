package oidc

import (
	"context"
	"errors"
	"time"

	central "github.com/unified-identity-auth-platform/data-analysis/internal/sessiongovernance"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrUnauthenticated = errors.New("analysis session is not authenticated")
var ErrSessionUnavailable = auth.ErrSessionUnavailable

// centralExpectation 固定中央响应的应用、租户和环境边界。
// 参数：无。
// 返回值：组合根配置的精确范围。
func (s *Service) centralExpectation() central.Expectation {
	return central.Expectation{TenantID: s.tenantID, ClientID: s.clientID, ApplicationCode: s.applicationCode, EnvironmentCode: s.environmentCode}
}

// readCentral 只读取或代理真实活动，不从本地last_seen推算中央周期。
// 参数：ctx为请求，session为加密服务器会话，interact表示通过同源保护的真实交互。
// 返回值：权威窗口；旧无绑定记录及确定失效要求重登，临时故障保持503语义。
func (s *Service) readCentral(ctx context.Context, session Session, interact bool) (central.Context, error) {
	if len(session.CentralBindingCipher) == 0 {
		return central.Context{}, ErrUnauthenticated
	}
	binding, err := s.codec.decrypt(session.CentralBindingCipher)
	if err != nil || binding == "" {
		return central.Context{}, ErrUnauthenticated
	}
	var state central.Context
	if interact {
		state, err = s.centralClient.Activity(ctx, binding)
	} else {
		state, err = s.centralClient.State(ctx, binding)
	}
	if err == nil {
		err = central.Validate(state, s.centralExpectation(), session.PlatformUserID)
	}
	if errors.Is(err, central.ErrInactive) || errors.Is(err, central.ErrBindingConflict) {
		return central.Context{}, ErrUnauthenticated
	}
	if err != nil {
		return central.Context{}, ErrSessionUnavailable
	}
	if state.SessionID != session.CentralSessionID {
		return central.Context{}, ErrUnauthenticated
	}
	return state, nil
}

// authenticateCentral 先验证唯一中央周期，再在行锁内刷新短令牌及授权快照。
// 参数：ctx为请求，cookie为当前HttpOnly Cookie原文。
// 返回值：有效数析主体；失效与临时故障分别返回未认证和依赖错误。
func (s *Service) authenticateCentral(ctx context.Context, cookie string) (auth.Principal, error) {
	if cookie == "" {
		return auth.Principal{}, ErrUnauthenticated
	}
	now := time.Now().UTC()
	var session Session
	err := s.db.WithContext(ctx).Where("session_id_hash = ? AND revoked_at IS NULL AND expires_at > ?", tokenHash(cookie), now).Take(&session).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return auth.Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return auth.Principal{}, ErrSessionUnavailable
	}
	if session.TenantID != s.tenantID {
		return auth.Principal{}, ErrUnauthenticated
	}
	if _, err = s.readCentral(ctx, session, false); err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			if revokeErr := s.db.WithContext(ctx).Model(&Session{}).Where("session_id_hash = ? AND revoked_at IS NULL", session.SessionIDHash).Update("revoked_at", now).Error; revokeErr != nil {
				return auth.Principal{}, ErrSessionUnavailable
			}
		}
		return auth.Principal{}, err
	}
	if !session.AccessTokenExpiresAt.After(now.Add(time.Minute)) || now.Sub(session.AuthorizationCheckedAt) >= s.checkEvery {
		session, err = s.refreshCentralSession(ctx, session.SessionIDHash, now)
		if err != nil {
			return auth.Principal{}, err
		}
	}
	return principalFromCentralSession(session), nil
}

// refreshCentralSession 先提交已验签轮换，再独立核对权限，防止权限503丢失单次refresh。
// 参数：ctx为原请求，hash为Cookie摘要，now为本次验证时钟。
// 返回值：最新会话；权限失败返回错误且保留第一事务的凭据。
func (s *Service) refreshCentralSession(ctx context.Context, hash string, now time.Time) (Session, error) {
	rotationCtx, cancel := central.RotationContext(ctx)
	_, err := s.withCentralSessionLock(rotationCtx, hash, now, s.rotateCentralCredentials)
	cancel()
	if err != nil {
		return Session{}, err
	}
	return s.withCentralSessionLock(ctx, hash, now, s.authorizeCentralCredentials)
}

// withCentralSessionLock 重读并串行执行一个认证阶段，绝不让普通读覆盖新凭据。
// 参数：ctx为该阶段上下文，hash为Cookie摘要，now为起始时间，operation为内部认证阶段。
// 返回值：该阶段已提交记录；明确失效提交当前行撤销，其他故障回滚。
func (s *Service) withCentralSessionLock(ctx context.Context, hash string, now time.Time, operation func(context.Context, *Session, time.Time) error) (Session, error) {
	var session Session
	var rejection error
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("session_id_hash = ? AND revoked_at IS NULL AND expires_at > ?", hash, now).Take(&session).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrUnauthenticated
			}
			return err
		}
		if _, err := s.readCentral(ctx, session, false); err != nil {
			if errors.Is(err, ErrUnauthenticated) {
				rejection = err
				return tx.Model(&Session{}).Where("session_id_hash = ?", hash).Update("revoked_at", time.Now().UTC()).Error
			}
			return err
		}
		lockedNow := time.Now().UTC()
		if session.AccessTokenExpiresAt.After(lockedNow.Add(time.Minute)) && lockedNow.Sub(session.AuthorizationCheckedAt) < s.checkEvery {
			return nil
		}
		if err := operation(ctx, &session, lockedNow); err != nil {
			if errors.Is(err, ErrUnauthenticated) {
				rejection = err
				return tx.Model(&Session{}).Where("session_id_hash = ?", hash).Update("revoked_at", lockedNow).Error
			}
			return err
		}
		return tx.Model(&Session{}).Where("session_id_hash = ?", hash).Updates(map[string]any{
			"access_token_cipher": session.AccessTokenCipher, "refresh_token_cipher": session.RefreshTokenCipher,
			"access_token_expires_at": session.AccessTokenExpiresAt, "roles_json": session.RolesJSON,
			"permissions_json": session.PermissionsJSON, "data_scopes_json": session.DataScopesJSON,
			"authz_revision": session.AuthzRevision, "authorization_checked_at": session.AuthorizationCheckedAt,
			"last_seen_at": lockedNow,
		}).Error
	})
	if errors.Is(err, ErrUnauthenticated) || rejection != nil {
		return Session{}, ErrUnauthenticated
	}
	if err != nil {
		return Session{}, ErrSessionUnavailable
	}
	return session, nil
}

// principalFromCentralSession 按原数据范围规则生成数析主体，不引入平台控制台权限。
// 参数：session为已通过中央和业务授权校验的持久会话。
// 返回值：数析独立授权主体。
func principalFromCentralSession(session Session) auth.Principal {
	mode, organizations, teams := principalScope(session.DataScopes(), session.Roles())
	return auth.Principal{UserID: session.PlatformUserID, TenantID: session.TenantID, Username: session.Username,
		DisplayName: session.DisplayName, Roles: session.Roles(), Permissions: session.Permissions(), ScopeMode: mode,
		OrganizationIDs: organizations, TeamIDs: teams, RoleConfigHash: session.RoleConfigHash,
		AuthzRevision: session.AuthzRevision, ExpiresAt: session.ExpiresAt.Unix()}
}
