package oidc

import (
	"context"
	"encoding/json"
	"errors"
	provider "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"time"
)

// rotateCentralCredentials 验签原身份并轮换凭据，提交前不依赖业务权限服务。
// 参数：ctx为有界轮换上下文，session为锁内记录，now为服务器时钟。
// 返回值：原地更新密文；拒绝或临时故障保持对应错误。
func (s *Service) rotateCentralCredentials(ctx context.Context, session *Session, now time.Time) error {
	access, err := s.codec.decrypt(session.AccessTokenCipher)
	if err != nil || access == "" {
		return ErrUnauthenticated
	}
	if !session.AccessTokenExpiresAt.After(now.Add(time.Minute)) {
		refresh, err := s.codec.decrypt(session.RefreshTokenCipher)
		if err != nil || refresh == "" {
			return ErrUnauthenticated
		}
		token, err := s.config.TokenSource(provider.ClientContext(ctx, s.httpClient), &oauth2.Token{RefreshToken: refresh, Expiry: now.Add(-time.Minute)}).Token()
		if err != nil {
			var protocol *oauth2.RetrieveError
			if errors.As(err, &protocol) && (protocol.ErrorCode == "invalid_grant" || protocol.ErrorCode == "invalid_token") {
				return ErrUnauthenticated
			}
			return ErrSessionUnavailable
		}
		if token.AccessToken == "" || !token.Expiry.After(time.Now().UTC()) {
			return ErrSessionUnavailable
		}
		if raw, ok := token.Extra("id_token").(string); ok && raw != "" {
			id, err := s.verifier.Verify(provider.ClientContext(ctx, s.httpClient), raw)
			if err != nil {
				return ErrUnauthenticated
			}
			var claims idTokenClaims
			if id.Claims(&claims) != nil || claims.TokenUse != "id_token" || claims.Sub != session.OIDCSubject || claims.IdentityID != session.PlatformUserID || claims.SessionID != session.OIDCSessionID || claims.TenantID != session.TenantID {
				return ErrUnauthenticated
			}
		}
		rotated := token.RefreshToken
		if rotated == "" {
			rotated = refresh
		}
		session.RefreshTokenCipher, err = s.codec.encrypt(rotated)
		if err != nil {
			return ErrSessionUnavailable
		}
		session.AccessTokenCipher, err = s.codec.encrypt(token.AccessToken)
		if err != nil {
			return ErrSessionUnavailable
		}
		session.AccessTokenExpiresAt = token.Expiry.UTC()
		session.AuthorizationCheckedAt = time.Unix(1, 0).UTC()
	}
	return nil
}

// authorizeCentralCredentials 使用第一事务已提交的访问令牌核验当前业务权限。
// 参数：ctx为原始请求上下文，session为第二事务最新记录，now为当前时钟。
// 返回值：成功更新授权快照；失败不重新刷新或回滚已保存的令牌。
func (s *Service) authorizeCentralCredentials(ctx context.Context, session *Session, now time.Time) error {
	access, err := s.codec.decrypt(session.AccessTokenCipher)
	if err != nil {
		return ErrUnauthenticated
	}
	value, err := s.resolveAuthorizationContext(ctx, access)
	if err != nil {
		return err
	}
	if _, err = validateAuthorizationBinding(value, session.OIDCSubject, session.PlatformUserID, session.TenantID, s.clientID, s.applicationCode, s.environmentCode); err != nil {
		return ErrUnauthenticated
	}
	if err = validateCatalogWindow(value, s.catalogVersion, s.roleConfigHash); err != nil {
		return ErrUnauthenticated
	}
	roles, err := json.Marshal(value.Roles)
	if err != nil {
		return ErrSessionUnavailable
	}
	permissions, err := json.Marshal(value.Permissions)
	if err != nil {
		return ErrSessionUnavailable
	}
	scopes, err := json.Marshal(value.DataScopes)
	if err != nil {
		return ErrSessionUnavailable
	}
	session.RolesJSON = string(roles)
	session.PermissionsJSON = string(permissions)
	session.DataScopesJSON = string(scopes)
	session.AuthzRevision = value.AuthorizationRevision
	session.AuthorizationCheckedAt = now
	return nil
}
