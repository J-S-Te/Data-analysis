package oidc

import (
	"context"
	"errors"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/authorizationrefresh"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// refreshLegacySession 在事务行锁内重读并复核旧模式授权，阻止迟到快照回填。
// 参数：ctx为请求，hash为已经核验的Cookie摘要，锁内再次验证全部会话期限。
// 返回值：最新持久快照；401或403保持本地安全失效，依赖故障503并回滚。
func (s *Service) refreshLegacySession(ctx context.Context, hash string) (Session, error) {
	var session Session
	var rejection error
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("session_id_hash = ?", hash).Take(&session).Error; err != nil {
			return err
		}
		now := time.Now().UTC()
		if !session.ValidAt(now, s.sessionIdleTTL) {
			return ErrUnauthenticated
		}
		if !authorizationrefresh.Forced(ctx) && now.Sub(session.AuthorizationCheckedAt) <= s.checkEvery {
			return nil
		}
		if err := s.authorizeCentralCredentials(ctx, &session, now); err != nil {
			if errors.Is(err, ErrUnauthenticated) || errors.Is(err, auth.ErrAuthorizationDenied) {
				rejection = err
				return tx.Model(&Session{}).Where("session_id_hash = ? AND revoked_at IS NULL", hash).Update("revoked_at", now).Error
			}
			return err
		}
		return tx.Model(&Session{}).Where("session_id_hash = ? AND revoked_at IS NULL", hash).Updates(map[string]any{
			"roles_json": session.RolesJSON, "permissions_json": session.PermissionsJSON, "data_scopes_json": session.DataScopesJSON,
			"authz_revision": session.AuthzRevision, "authorization_checked_at": session.AuthorizationCheckedAt, "last_seen_at": now,
		}).Error
	})
	if err == nil && rejection != nil {
		return Session{}, rejection
	}
	if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, ErrUnauthenticated) {
		return Session{}, ErrUnauthenticated
	}
	if errors.Is(err, auth.ErrAuthorizationDenied) {
		return Session{}, auth.ErrAuthorizationDenied
	}
	if err != nil {
		return Session{}, ErrSessionUnavailable
	}
	return session, nil
}
