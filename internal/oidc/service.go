package oidc

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"gorm.io/gorm"

	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

// Options 是 OIDC 客户端与会话服务的装配参数。
type Options struct {
	Issuer            string // 浏览器可达的 Keycloak Realm 地址
	BackchannelIssuer string // 容器内 Discovery/JWKS/Token/UserInfo 地址（可空则用 Issuer）
	ClientID          string
	ClientSecret      string
	RedirectURL       string
	TenantID          string
	PathPrefix        string
	SessionTTL        time.Duration
	CodecKey          []byte // 32 字节
	// AuthorizationContextURL 平台授权上下文端点（对齐 contract：OIDC 只认证身份，
	// 角色/权限由平台 /oauth2/authorization-context 在线解析，不读 token claims）。
	AuthorizationContextURL string
	// 授权复核窗口：超过该时长未复核则调用授权上下文在线复核（接入手册 §3.3）
	AuthorizationCheckInterval time.Duration
}

type Service struct {
	provider                *oidc.Provider
	verifier                *oidc.IDTokenVerifier
	config                  oauth2.Config
	httpClient              *http.Client
	issuer                  string
	redirect                string
	tenantID                string
	pathPrefix              string
	sessionTTL              time.Duration
	codec                   *secretCodec
	db                      *gorm.DB
	checkEvery              time.Duration
	authorizationContextURL string
}

func NewService(ctx context.Context, db *gorm.DB, options Options) (*Service, error) {
	// 公网 issuer 必须原样用于 discovery（go-oidc 校验返回的 issuer 一致）；
	// 容器内通过 backchannel 主机重写实际连接目标（对齐 contract OIDC_BACKCHANNEL_BASE_URL 模式）。
	providerCtx := ctx
	httpClient := &http.Client{Timeout: 5 * time.Second}
	if options.BackchannelIssuer != "" {
		transport, err := newHostRewriteTransport(options.BackchannelIssuer)
		if err != nil {
			return nil, err
		}
		httpClient.Transport = transport
		providerCtx = oidc.ClientContext(ctx, httpClient)
	}
	provider, err := oidc.NewProvider(providerCtx, options.Issuer)
	if err != nil {
		return nil, err
	}
	verifier := provider.Verifier(&oidc.Config{ClientID: options.ClientID})
	codec, err := newSecretCodec(options.CodecKey)
	if err != nil {
		return nil, err
	}
	redirect := options.RedirectURL
	if redirect == "" {
		redirect = strings.TrimSuffix(options.Issuer, "/") + "/auth/callback"
	}
	sessionTTL := options.SessionTTL
	if sessionTTL <= 0 {
		sessionTTL = 15 * time.Minute
	}
	checkEvery := options.AuthorizationCheckInterval
	if checkEvery <= 0 {
		checkEvery = 5 * time.Minute
	}
	return &Service{
		provider: provider,
		verifier: verifier,
		config: oauth2.Config{
			ClientID:     options.ClientID,
			ClientSecret: options.ClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  redirect,
			Scopes:       []string{oidc.ScopeOpenID, "profile"},
		},
		issuer:                  options.Issuer,
		redirect:                redirect,
		tenantID:                options.TenantID,
		pathPrefix:              options.PathPrefix,
		sessionTTL:              sessionTTL,
		codec:                   codec,
		db:                      db,
		checkEvery:              checkEvery,
		httpClient:              httpClient,
		authorizationContextURL: options.AuthorizationContextURL,
	}, nil
}

// idTokenClaims 平台 ID Token 携带的声明（对齐 contract：OIDC 只认证身份，
// 角色/权限/数据范围由平台 authorization-context 在线解析，不读 token claims）。
type idTokenClaims struct {
	Sub               string `json:"sub"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
	TenantID          string `json:"tenant_id"`
	TokenUse          string `json:"token_use"`
}

// AuthorizationContext 平台授权上下文响应（对齐 contract authorization_context.go）。
type AuthorizationContext struct {
	Subject               string   `json:"sub"`
	IdentityID            string   `json:"identity_id"`
	TenantID              string   `json:"tenant_id"`
	ApplicationCode       string   `json:"application_code"`
	EnvironmentCode       string   `json:"environment_code"`
	Roles                 []string `json:"roles"`
	Permissions           []string `json:"permissions"`
	AuthorizationRevision uint64   `json:"authorization_revision"`
}

// AuthorizationURL 生成 Keycloak 授权地址（Authorization Code + PKCE S256）。
func (s *Service) AuthorizationURL(state string, nonce string, codeVerifier string) string {
	nonceParam := oauth2.SetAuthURLParam("nonce", nonce)
	return s.config.AuthCodeURL(state, oauth2.S256ChallengeOption(codeVerifier), nonceParam)
}

// ExchangeAndCreateSession 校验回调 code，验证 ID Token，创建服务端会话，返回会话 token 与展示信息。
func (s *Service) ExchangeAndCreateSession(ctx context.Context, code string, codeVerifier string, expectedNonce string, transaction LoginTransaction) (sessionToken string, session Session, err error) {
	ctx = context.WithValue(ctx, oauth2.HTTPClient, s.httpClient)
	rawToken, err := s.config.Exchange(ctx, code, oauth2.VerifierOption(codeVerifier))
	if err != nil {
		return "", Session{}, err
	}
	idToken, ok := rawToken.Extra("id_token").(string)
	if !ok || idToken == "" {
		return "", Session{}, errors.New("id_token missing")
	}
	verified, err := s.verifier.Verify(ctx, idToken)
	if err != nil {
		return "", Session{}, err
	}
	var claims idTokenClaims
	if err := verified.Claims(&claims); err != nil {
		return "", Session{}, err
	}
	if claims.TokenUse != "" && claims.TokenUse != "id_token" {
		return "", Session{}, errors.New("token_use must be id_token")
	}
	if expectedNonce != "" && claims.Sub == "" {
		return "", Session{}, errors.New("nonce claim missing")
	}
	// 拒绝未知角色/权限由目录哈希与本地 manifest 校验完成（启动时 ValidateClaimsRoleConfigHash）；
	// 这里至少拒绝跨租户会话。
	if s.tenantID != "" && claims.TenantID != "" && claims.TenantID != s.tenantID {
		return "", Session{}, errors.New("tenant mismatch")
	}
	accessTokenCipher, err := s.codec.encrypt(rawToken.AccessToken)
	if err != nil {
		return "", Session{}, err
	}
	// 角色/权限由平台授权上下文在线解析（对齐 contract：不读 token claims）。
	authz, err := s.resolveAuthorizationContext(ctx, rawToken.AccessToken)
	if err != nil {
		slog.Warn("resolve authorization context failed", "error", err.Error(), "url", s.authorizationContextURL)
		return "", Session{}, fmt.Errorf("resolve authorization context: %w", err)
	}
	rolesJSON, _ := json.Marshal(authz.Roles)
	permissionsJSON, _ := json.Marshal(authz.Permissions)
	now := time.Now().UTC()
	sessionToken, err = randomToken(32)
	if err != nil {
		return "", Session{}, err
	}
	session = Session{
		SessionIDHash:          tokenHash(sessionToken),
		TenantID:               authz.TenantID,
		PlatformUserID:         claims.Sub,
		DisplayName:            claims.Name,
		RolesJSON:              string(rolesJSON),
		PermissionsJSON:        string(permissionsJSON),
		RoleConfigHash:         "",
		AuthzRevision:          authz.AuthorizationRevision,
		AccessTokenCipher:      accessTokenCipher,
		ExpiresAt:              now.Add(s.sessionTTL),
		AuthorizationCheckedAt: now,
		CreatedAt:              now,
		LastSeenAt:             now,
	}
	if err := s.db.Create(&session).Error; err != nil {
		return "", Session{}, err
	}
	return sessionToken, session, nil
}

// resolveAuthorizationContext 调平台授权上下文端点（Bearer Keycloak access token）解析角色/权限。
func (s *Service) resolveAuthorizationContext(ctx context.Context, accessToken string) (AuthorizationContext, error) {
	if s.authorizationContextURL == "" {
		return AuthorizationContext{}, errors.New("authorization context endpoint is not configured")
	}
	if accessToken == "" {
		return AuthorizationContext{}, errors.New("access token is missing")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.authorizationContextURL, nil)
	if err != nil {
		return AuthorizationContext{}, err
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Accept", "application/json")
	// 注意：不要复用 OIDC 的 hostRewriteTransport（它会重写 host 到 Keycloak）；
	// authorization-context 是平台 API（platform-api:8080），容器内直连即可。
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return AuthorizationContext{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return AuthorizationContext{}, fmt.Errorf("authorization context returned HTTP %d", response.StatusCode)
	}
	var result AuthorizationContext
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&result); err != nil {
		return AuthorizationContext{}, err
	}
	if result.Subject == "" {
		return AuthorizationContext{}, errors.New("authorization context missing subject")
	}
	slog.Info("authorization context resolved",
		"sub", result.Subject, "application_code", result.ApplicationCode,
		"roles", result.Roles, "permissions", result.Permissions, "revision", result.AuthorizationRevision)
	return result, nil
}

// Authenticate 由 cookie token 还原 Principal；必要时执行 UserInfo 在线复核。
func (s *Service) Authenticate(ctx context.Context, cookieValue string) (auth.Principal, error) {
	if cookieValue == "" {
		return auth.Principal{}, errors.New("empty session")
	}
	var session Session
	err := s.db.Where("session_id_hash = ?", tokenHash(cookieValue)).First(&session).Error
	if err != nil {
		return auth.Principal{}, errors.New("session not found")
	}
	if session.RevokedAt != nil || time.Now().UTC().After(session.ExpiresAt) {
		return auth.Principal{}, errors.New("session revoked or expired")
	}
	if time.Since(session.AuthorizationCheckedAt) > s.checkEvery {
		// 定期调平台授权上下文在线复核：角色/权限变更后旧会话自动刷新（接入手册 §3.3）。
		// 复核失败时 fail-open 保留旧权限并记录（不销毁会话），避免授权服务抖动导致误登出。
		now := time.Now().UTC()
		if accessToken, decryptErr := s.codec.decrypt(session.AccessTokenCipher); decryptErr == nil {
			if authz, resolveErr := s.resolveAuthorizationContext(ctx, accessToken); resolveErr == nil {
				rolesJSON, _ := json.Marshal(authz.Roles)
				permissionsJSON, _ := json.Marshal(authz.Permissions)
				_ = s.db.Model(&Session{}).Where("session_id_hash = ?", session.SessionIDHash).
					Updates(map[string]interface{}{
						"roles_json": string(rolesJSON), "permissions_json": string(permissionsJSON),
						"authz_revision":           authz.AuthorizationRevision,
						"authorization_checked_at": now, "last_seen_at": now,
					}).Error
				session.RolesJSON = string(rolesJSON)
				session.PermissionsJSON = string(permissionsJSON)
				session.AuthzRevision = authz.AuthorizationRevision
			}
		}
		session.AuthorizationCheckedAt = now
		session.LastSeenAt = now
	}
	return auth.Principal{
		UserID:         session.PlatformUserID,
		TenantID:       session.TenantID,
		DisplayName:    session.DisplayName,
		Roles:          session.Roles(),
		Permissions:    session.Permissions(),
		RoleConfigHash: session.RoleConfigHash,
		AuthzRevision:  session.AuthzRevision,
		ExpiresAt:      session.ExpiresAt.Unix(),
	}, nil
}

// RevokeSession 登出撤销服务端会话。
func (s *Service) RevokeSession(cookieValue string) error {
	if cookieValue == "" {
		return nil
	}
	now := time.Now().UTC()
	return s.db.Model(&Session{}).Where("session_id_hash = ?", tokenHash(cookieValue)).
		Update("revoked_at", now).Error
}

// SaveLoginTransaction 保存登录事务（state 单次消费）。
func (s *Service) SaveLoginTransaction(state string, nonce string, codeVerifier string, returnPath string) error {
	nonceCipher, err := s.codec.encrypt(nonce)
	if err != nil {
		return err
	}
	verifierCipher, err := s.codec.encrypt(codeVerifier)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	return s.db.Create(&LoginTransaction{
		StateHash:          tokenHash(state),
		TenantID:           s.tenantID,
		NonceCipher:        nonceCipher,
		CodeVerifierCipher: verifierCipher,
		ReturnPath:         returnPath,
		ExpiresAt:          now.Add(10 * time.Minute),
		CreatedAt:          now,
	}).Error
}

// ConsumeLoginTransaction 读取并删除登录事务（单次消费，防重放）。
func (s *Service) ConsumeLoginTransaction(ctx context.Context, state string) (LoginTransaction, error) {
	var transaction LoginTransaction
	err := s.db.Where("state_hash = ?", tokenHash(state)).First(&transaction).Error
	if err != nil {
		return LoginTransaction{}, err
	}
	if time.Now().UTC().After(transaction.ExpiresAt) {
		return LoginTransaction{}, errors.New("login transaction expired")
	}
	if err := s.db.Delete(&transaction).Error; err != nil {
		return LoginTransaction{}, err
	}
	return transaction, nil
}

func (s *Service) DecryptNonce(cipher []byte) (string, error)    { return s.codec.decrypt(cipher) }
func (s *Service) DecryptVerifier(cipher []byte) (string, error) { return s.codec.decrypt(cipher) }

func (s *Service) PathPrefix() string  { return s.pathPrefix }
func (s *Service) RedirectURL() string { return s.redirect }

// EndSessionEndpoint 从 provider 发现登出端点（无则空，回退本地登出）。
func (s *Service) EndSessionEndpoint(ctx context.Context) (string, error) {
	var metadata struct {
		EndSessionEndpoint string `json:"end_session_endpoint"`
	}
	if err := s.provider.Claims(&metadata); err != nil {
		return "", err
	}
	return metadata.EndSessionEndpoint, nil
}

// GenerateState 生成安全随机 state。
func GenerateState() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// BuildRedirectURL 拼接同源登出后回调地址（只允许同源）。
func BuildRedirectURL(base string, path string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	u.Path = path
	return u.String()
}
