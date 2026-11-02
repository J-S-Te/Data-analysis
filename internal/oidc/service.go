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
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"gorm.io/gorm"

	"github.com/unified-identity-auth-platform/data-analysis/internal/platformaudit"
	central "github.com/unified-identity-auth-platform/data-analysis/internal/sessiongovernance"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

// 会话双期限默认值与边界，对应 OIDC_SESSION_IDLE_TTL / OIDC_SESSION_ABSOLUTE_TTL。
// 配置装载与 NewService 共用同一校验，非法值一律失败关闭。
const (
	DefaultSessionIdleTTL     = 30 * time.Minute
	DefaultSessionAbsoluteTTL = 8 * time.Hour
	MinSessionIdleTTL         = time.Minute
	MaxSessionIdleTTL         = 24 * time.Hour
)

// ErrSessionInactive 表示会话已撤销、空闲到期或绝对到期；这类会话不得通过活动接口复活。
var ErrSessionInactive = errors.New("session is inactive")

// ValidateSessionTTLs 校验会话双期限：空闲期限必须在 1m~24h，绝对期限必须大于空闲期限。
// 参数：idle 为空闲期限，absolute 为绝对期限。
// 返回值：合法返回 nil；非法返回错误（启动失败关闭，避免退化为永久在线或立即过期）。
func ValidateSessionTTLs(idle, absolute time.Duration) error {
	if idle < MinSessionIdleTTL || idle > MaxSessionIdleTTL {
		return errors.New("OIDC_SESSION_IDLE_TTL must be between 1m and 24h")
	}
	if absolute <= idle {
		return errors.New("OIDC_SESSION_ABSOLUTE_TTL must be longer than OIDC_SESSION_IDLE_TTL")
	}
	return nil
}

// sessionLifetimes 是新登录会话的时间边界，单独成结构便于登录装配与单测共用。
type sessionLifetimes struct {
	ExpiresAt         time.Time
	LastInteractiveAt time.Time
}

// Options 是 OIDC 客户端与会话服务的装配参数。
type Options struct {
	CentralClient     central.Client
	Issuer            string // 浏览器可达的 Keycloak Realm 地址
	BackchannelIssuer string // 容器内 Discovery/JWKS/Token/UserInfo 地址（可空则用 Issuer）
	ClientID          string
	ClientSecret      string
	RedirectURL       string
	TenantID          string
	ApplicationCode   string
	EnvironmentCode   string
	CatalogVersion    string
	RoleConfigHash    string
	PathPrefix        string
	// SessionIdleTTL 是浏览器空闲期限：只有显式活动接口（POST /api/v1/auth/activity）续期，
	// 普通业务请求不续期。
	SessionIdleTTL time.Duration
	// SessionAbsoluteTTL 是登录后的绝对期限：不受活动续期影响，Cookie 到期与之一致。
	SessionAbsoluteTTL time.Duration
	CodecKey           []byte // 32 字节
	// AuthorizationContextURL 平台授权上下文端点（对齐 contract：OIDC 只认证身份，
	// 角色/权限由平台 /oauth2/authorization-context 在线解析，不读 token claims）。
	AuthorizationContextURL string
	// 授权复核窗口：超过该时长未复核则调用授权上下文在线复核（接入手册 §3.3）
	AuthorizationCheckInterval time.Duration
	// Reporter 可选；非空时在登录/登出后向平台审计上报 auth.login / auth.logout。
	Reporter platformaudit.Reporter
}

type Service struct {
	centralClient           central.Client
	provider                *oidc.Provider
	verifier                *oidc.IDTokenVerifier
	config                  oauth2.Config
	httpClient              *http.Client
	issuer                  string
	redirect                string
	tenantID                string
	clientID                string
	applicationCode         string
	environmentCode         string
	catalogVersion          string
	roleConfigHash          string
	pathPrefix              string
	sessionIdleTTL          time.Duration
	sessionAbsoluteTTL      time.Duration
	codec                   *secretCodec
	db                      *gorm.DB
	checkEvery              time.Duration
	authorizationContextURL string
	reporter                platformaudit.Reporter
}

// NewService 装配会话服务与可选中央权威依赖。
// 参数：ctx为调用上下文与取消边界。
//
//	db为本应用数据库连接。
//	options为本应用认证及运行选项。
//
// 返回值：装配后的应用或认证实例；配置与依赖装配失败时返回错误。
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
	providerCtx = oidc.ClientContext(ctx, httpClient)
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
	sessionIdleTTL := options.SessionIdleTTL
	if sessionIdleTTL <= 0 {
		sessionIdleTTL = DefaultSessionIdleTTL
	}
	sessionAbsoluteTTL := options.SessionAbsoluteTTL
	if sessionAbsoluteTTL <= 0 {
		sessionAbsoluteTTL = DefaultSessionAbsoluteTTL
	}
	// 失败关闭：绝对期限必须大于空闲期限，否则“活动续期”语义不成立。
	if err := ValidateSessionTTLs(sessionIdleTTL, sessionAbsoluteTTL); err != nil {
		return nil, err
	}
	checkEvery := options.AuthorizationCheckInterval
	if checkEvery <= 0 {
		checkEvery = 5 * time.Minute
	}
	return &Service{
		centralClient: options.CentralClient,
		provider:      provider,
		verifier:      verifier,
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
		clientID:                options.ClientID,
		applicationCode:         options.ApplicationCode,
		environmentCode:         options.EnvironmentCode,
		catalogVersion:          strings.TrimSpace(options.CatalogVersion),
		roleConfigHash:          strings.TrimSpace(options.RoleConfigHash),
		pathPrefix:              options.PathPrefix,
		sessionIdleTTL:          sessionIdleTTL,
		sessionAbsoluteTTL:      sessionAbsoluteTTL,
		codec:                   codec,
		db:                      db,
		checkEvery:              checkEvery,
		httpClient:              httpClient,
		authorizationContextURL: options.AuthorizationContextURL,
		reporter:                options.Reporter,
	}, nil
}

// idTokenClaims 平台 ID 令牌 携带的声明（对齐 contract：OIDC 只认证身份，
// 角色/权限/数据范围由平台 authorization-context 在线解析，不读 token claims）。
type idTokenClaims struct {
	IdentityID        string `json:"identity_id"`
	SessionID         string `json:"sid"`
	Sub               string `json:"sub"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
	TenantID          string `json:"tenant_id"`
	Nonce             string `json:"nonce"`
	TokenUse          string `json:"token_use"`
}

// AuthorizationContext 平台授权上下文响应（对齐 contract authorization_context.go）。
type DataScope struct {
	RoleCode        string `json:"role_code"`
	ScopeType       string `json:"scope_type"`
	ScopeID         string `json:"scope_id"`
	EnvironmentCode string `json:"environment_code"`
}

type AuthorizationContext struct {
	Subject                    string      `json:"sub"`
	SubjectID                  string      `json:"subject_id"`
	IdentityID                 string      `json:"identity_id"`
	TenantID                   string      `json:"tenant_id"`
	ClientID                   string      `json:"client_id"`
	ApplicationCode            string      `json:"application_code"`
	EnvironmentCode            string      `json:"environment_code"`
	Roles                      []string    `json:"roles"`
	Permissions                []string    `json:"permissions"`
	CatalogVersion             string      `json:"catalog_version"`
	CompatibleCatalogVersions  []string    `json:"compatible_catalog_versions"`
	RoleConfigHash             string      `json:"role_config_hash"`
	CompatibleRoleConfigHashes []string    `json:"compatible_role_config_hashes"`
	AuthorizationRevision      uint64      `json:"authorization_revision"`
	DataScopes                 []DataScope `json:"data_scopes"`
}

// AuthorizationURL 保留原登录事务构造提供方授权地址。
//
//	AuthorizationURL 生成 Keycloak 授权地址（Authorization Code + PKCE S256）。
//
// 参数：state为原始登录事务随机状态。
//
//	nonce为原OIDC登录随机校验材料。
//	codeVerifier为原PKCE秘密。
//
// 返回值：绑定原登录事务的授权地址；构造失败按原错误模型返回。
func (s *Service) AuthorizationURL(state string, nonce string, codeVerifier string) string {
	nonceParam := oauth2.SetAuthURLParam("nonce", nonce)
	if s.centralClient != nil {
		return s.config.AuthCodeURL(state, oauth2.S256ChallengeOption(codeVerifier), nonceParam, oauth2.SetAuthURLParam("claims", `{"id_token":{"auth_time":{"essential":true}}}`))
	}
	return s.config.AuthCodeURL(state, oauth2.S256ChallengeOption(codeVerifier), nonceParam)
}

// ExchangeAndCreateSession 交换原授权码并创建受中央固定期限约束的会话。
//
//	ExchangeAndCreateSession 校验回调 code，验证 ID 令牌，创建服务端会话，返回会话 token 与展示信息。
//
// 参数：ctx为调用上下文与取消边界。
//
//	code为一次性OIDC授权码。
//	codeVerifier为原PKCE秘密。
//	expectedNonce为原始登录事务保存的nonce。
//	transaction为原始一次性登录事务。
//
// 返回值：受原始期限约束的登录结果；身份、绑定或依赖失败返回错误。
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
	if err := validateIDTokenClaims(claims, expectedNonce, s.tenantID, transaction.TenantID); err != nil {
		return "", Session{}, err
	}
	var binding central.Binding
	if s.centralClient != nil {
		ticket, unwrapErr := s.codec.decrypt(transaction.CentralTicketCipher)
		if unwrapErr != nil || ticket == "" || claims.SessionID == "" || claims.IdentityID == "" || rawToken.RefreshToken == "" {
			return "", Session{}, ErrUnauthenticated
		}
		binding, err = s.centralClient.Complete(ctx, ticket, rawToken.AccessToken, idToken)
		if errors.Is(err, central.ErrUnavailable) {
			return "", Session{}, ErrSessionUnavailable
		}
		if err != nil {
			return "", Session{}, ErrUnauthenticated
		}
		if err = central.Validate(binding.Context, s.centralExpectation(), claims.IdentityID); err != nil {
			return "", Session{}, ErrUnauthenticated
		}
	}

	if strings.TrimSpace(claims.PreferredUsername) == "" {
		info, profileErr := s.provider.UserInfo(ctx, oauth2.StaticTokenSource(rawToken))
		if profileErr != nil {
			return "", Session{}, profileErr
		}
		var profile struct {
			Subject  string `json:"sub"`
			Username string `json:"preferred_username"`
		}
		if profileErr = info.Claims(&profile); profileErr != nil || profile.Subject != claims.Sub {
			return "", Session{}, errors.New("OIDC UserInfo subject mismatch")
		}
		claims.PreferredUsername = strings.TrimSpace(profile.Username)
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
	platformSubjectID, err := validateAuthorizationBinding(authz, claims.Sub, "", claims.TenantID, s.clientID, s.applicationCode, s.environmentCode)
	if err != nil {
		return "", Session{}, err
	}
	if err := validateCatalogWindow(authz, s.catalogVersion, s.roleConfigHash); err != nil {
		return "", Session{}, err
	}
	rolesJSON, _ := json.Marshal(authz.Roles)
	permissionsJSON, _ := json.Marshal(authz.Permissions)
	dataScopesJSON, _ := json.Marshal(authz.DataScopes)
	now := time.Now().UTC()
	sessionToken, err = randomToken(32)
	if err != nil {
		return "", Session{}, err
	}
	lifetimes := s.newSessionLifetimes(now)
	session = Session{
		SessionIDHash:          tokenHash(sessionToken),
		TenantID:               authz.TenantID,
		PlatformUserID:         platformSubjectID,
		DisplayName:            claims.Name,
		Username:               strings.TrimSpace(claims.PreferredUsername),
		RolesJSON:              string(rolesJSON),
		PermissionsJSON:        string(permissionsJSON),
		DataScopesJSON:         string(dataScopesJSON),
		RoleConfigHash:         "",
		AuthzRevision:          authz.AuthorizationRevision,
		AccessTokenCipher:      accessTokenCipher,
		ExpiresAt:              lifetimes.ExpiresAt,
		AuthorizationCheckedAt: now,
		CreatedAt:              now,
		LastSeenAt:             now,
		LastInteractiveAt:      &lifetimes.LastInteractiveAt,
	}
	if s.centralClient != nil {
		if platformSubjectID != binding.Context.SubjectID {
			return "", Session{}, ErrUnauthenticated
		}
		session.CentralBindingCipher, err = s.codec.encrypt(binding.Token)
		if err != nil {
			return "", Session{}, ErrSessionUnavailable
		}
		session.RefreshTokenCipher, err = s.codec.encrypt(rawToken.RefreshToken)
		if err != nil {
			return "", Session{}, ErrSessionUnavailable
		}
		session.CentralSessionID = binding.Context.SessionID
		session.ExpiresAt = binding.Context.AbsoluteExpiresAt
		session.AccessTokenExpiresAt = rawToken.Expiry.UTC()
		session.OIDCSubject = claims.Sub
		session.OIDCSessionID = claims.SessionID
	}
	if err := s.db.WithContext(ctx).Create(&session).Error; err != nil {
		return "", Session{}, err
	}
	s.reportAuth(ctx, platformaudit.Event{ActorID: session.PlatformUserID, ActorName: session.DisplayName, Action: "auth.login", ResourceType: "auth_session", Result: "SUCCESS", RiskLevel: "LOW"})
	return sessionToken, session, nil
}

// newSessionLifetimes 返回新登录会话的时间边界：绝对到期 = now + absolute，
// 空闲计时从登录时刻起算；此后只有显式活动接口会推进空闲计时。
// 参数：now 为登录成功时间。
// 返回值：绝对到期与最近显式活动时间。
func (s *Service) newSessionLifetimes(now time.Time) sessionLifetimes {
	return sessionLifetimes{ExpiresAt: now.Add(s.sessionAbsoluteTTL), LastInteractiveAt: now}
}

// reportAuth 向平台审计上报登录/登出事件；审计未配置或上报失败均不影响认证流程。
func (s *Service) reportAuth(ctx context.Context, event platformaudit.Event) {
	if s.reporter == nil {
		return
	}
	_ = s.reporter.Report(ctx, event)
}

// resolveAuthorizationContext 读取并核验基础平台当前应用的权限快照。
//
//	resolveAuthorizationContext 调平台授权上下文端点（Bearer Keycloak access token）解析角色/权限。
//
// 参数：ctx为调用上下文与取消边界。
//
//	accessToken为服务器端访问令牌。
//
// 返回值：与该调用匹配的核验或持久化结果；失败保留原有错误语义。
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
		if response.StatusCode == 401 || response.StatusCode == 403 {
			return AuthorizationContext{}, ErrUnauthenticated
		}
		return AuthorizationContext{}, ErrSessionUnavailable
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

// Authenticate 校验当前浏览器会话、中央期限与业务授权。
//
//	Authenticate 由 cookie token 还原 Principal；必要时执行 UserInfo 在线复核。
//
// 参数：ctx为调用上下文与取消边界。
//
//	cookieValue为当前浏览器随机会话材料。
//
// 返回值：经过核验的主体或会话；明确失效与暂时依赖错误分别返回。
func (s *Service) Authenticate(ctx context.Context, cookieValue string) (auth.Principal, error) {
	if s.centralClient != nil {
		return s.authenticateCentral(ctx, cookieValue)
	}
	if cookieValue == "" {
		return auth.Principal{}, errors.New("empty session")
	}
	var session Session
	err := s.db.Where("session_id_hash = ?", tokenHash(cookieValue)).First(&session).Error
	if err != nil {
		return auth.Principal{}, errors.New("session not found")
	}
	now := time.Now().UTC()
	if !session.ValidAt(now, s.sessionIdleTTL) {
		// 撤销、空闲到期与绝对到期一律拒绝：不得刷新任何时间字段（不能复活）。
		return auth.Principal{}, errors.New("session revoked, idle or expired")
	}
	if time.Since(session.AuthorizationCheckedAt) > s.checkEvery {
		// 定期调平台授权上下文在线复核：角色/权限变更后旧会话自动刷新（接入手册 §3.3）。
		// 复核失败必须 失败时安全拒绝，否则已撤销的权限会在旧会话中继续生效。
		refreshAt := time.Now().UTC()
		accessToken, decryptErr := s.codec.decrypt(session.AccessTokenCipher)
		if decryptErr != nil {
			return auth.Principal{}, errors.New("session access token is invalid")
		}
		authz, resolveErr := s.resolveAuthorizationContext(ctx, accessToken)
		if resolveErr != nil {
			return auth.Principal{}, fmt.Errorf("refresh authorization context: %w", resolveErr)
		}
		if _, bindErr := validateAuthorizationBinding(authz, "", session.PlatformUserID, session.TenantID, s.clientID, s.applicationCode, s.environmentCode); bindErr != nil {
			return auth.Principal{}, bindErr
		}
		if windowErr := validateCatalogWindow(authz, s.catalogVersion, s.roleConfigHash); windowErr != nil {
			return auth.Principal{}, windowErr
		}
		rolesJSON, _ := json.Marshal(authz.Roles)
		permissionsJSON, _ := json.Marshal(authz.Permissions)
		dataScopesJSON, _ := json.Marshal(authz.DataScopes)
		if updateErr := s.db.Model(&Session{}).Where("session_id_hash = ?", session.SessionIDHash).
			Updates(map[string]interface{}{
				"roles_json": string(rolesJSON), "permissions_json": string(permissionsJSON),
				"data_scopes_json":         string(dataScopesJSON),
				"authz_revision":           authz.AuthorizationRevision,
				"authorization_checked_at": refreshAt, "last_seen_at": refreshAt,
			}).Error; updateErr != nil {
			return auth.Principal{}, fmt.Errorf("persist refreshed authorization: %w", updateErr)
		}
		session.RolesJSON = string(rolesJSON)
		session.PermissionsJSON = string(permissionsJSON)
		session.DataScopesJSON = string(dataScopesJSON)
		session.AuthzRevision = authz.AuthorizationRevision
		session.AuthorizationCheckedAt = refreshAt
		session.LastSeenAt = refreshAt
	} else {
		// 普通已认证请求只刷新 last_seen_at（在线状态与审计辅助），绝不续期空闲计时；
		// 写失败不阻断业务请求，本次会话有效性以刚完成的读取与判定为准。
		_ = s.db.Model(&Session{}).Where("session_id_hash = ?", session.SessionIDHash).Update("last_seen_at", now).Error
		session.LastSeenAt = now
	}
	scopeMode, organizationIDs, teamIDs := principalScope(session.DataScopes(), session.Roles())
	return auth.Principal{
		UserID:          session.PlatformUserID,
		TenantID:        session.TenantID,
		DisplayName:     session.DisplayName,
		Username:        session.Username,
		Roles:           session.Roles(),
		Permissions:     session.Permissions(),
		ScopeMode:       scopeMode,
		OrganizationIDs: organizationIDs,
		TeamIDs:         teamIDs,
		RoleConfigHash:  session.RoleConfigHash,
		AuthzRevision:   session.AuthzRevision,
		ExpiresAt:       session.ExpiresAt.Unix(),
		// 空闲剩余取 min(空闲截止, 绝对截止) - now，前端据此在到期前回本子系统登录。
		IdleTimeoutSeconds: int64(s.sessionIdleTTL / time.Second),
		IdleRemainingMS:    session.IdleRemaining(now, s.sessionIdleTTL).Milliseconds(),
	}, nil
}

// ActivityResult 是显式活动续期的结果：空闲期限与续期后的剩余可用时长。
type ActivityResult struct {
	IdleTimeout   time.Duration
	IdleRemaining time.Duration
}

// TouchActivity 用浏览器显式活动（鼠标/键盘）续期会话的空闲计时。
// 仅当会话当前有效（未撤销、绝对未到期、空闲未到期）时更新 last_interactive_at 与
// last_seen_at；已失效会话返回 ErrSessionInactive，且不刷新任何字段（不得复活）。
// 参数：ctx 为请求上下文，cookieValue 为浏览器会话 cookie 原值。
// 返回值：续期结果或 ErrSessionInactive/数据库错误。
func (s *Service) TouchActivity(ctx context.Context, cookieValue string) (ActivityResult, error) {
	if strings.TrimSpace(cookieValue) == "" {
		return ActivityResult{}, ErrSessionInactive
	}
	now := time.Now().UTC()
	hash := tokenHash(cookieValue)
	var session Session
	if err := s.db.WithContext(ctx).Where("session_id_hash = ?", hash).First(&session).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ActivityResult{}, ErrSessionInactive
		}
		// 存储故障不能伪装成“会话失效”，否则前端会无谓重登；交给上层按 500 处理。
		return ActivityResult{}, err
	}
	if !session.ValidAt(now, s.sessionIdleTTL) {
		return ActivityResult{}, ErrSessionInactive
	}
	updated := s.db.WithContext(ctx).Model(&Session{}).
		Where("session_id_hash = ? AND revoked_at IS NULL AND expires_at > ?", hash, now).
		Updates(map[string]interface{}{"last_interactive_at": now, "last_seen_at": now})
	if updated.Error != nil {
		return ActivityResult{}, updated.Error
	}
	if updated.RowsAffected == 0 {
		// MySQL DATETIME(3) 只有毫秒精度：并发活动续期可能写入同一毫秒，MySQL 因值未变化
		// 报告 0 行。此时按权威有效条件复核，不能把合法并发请求误判为会话失效。
		var current Session
		if err := s.db.WithContext(ctx).Where("session_id_hash = ?", hash).First(&current).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ActivityResult{}, ErrSessionInactive
			}
			return ActivityResult{}, err
		}
		if !current.ValidAt(now, s.sessionIdleTTL) {
			return ActivityResult{}, ErrSessionInactive
		}
		session = current
	} else {
		session.LastInteractiveAt = &now
		session.LastSeenAt = now
	}
	return ActivityResult{IdleTimeout: s.sessionIdleTTL, IdleRemaining: session.IdleRemaining(now, s.sessionIdleTTL)}, nil
}

// RevokeSession 登出撤销服务端会话。
func (s *Service) RevokeSession(cookieValue string) error {
	if cookieValue == "" {
		return nil
	}
	now := time.Now().UTC()
	var session Session
	if err := s.db.Where("session_id_hash = ? AND revoked_at IS NULL", tokenHash(cookieValue)).First(&session).Error; err == nil && session.PlatformUserID != "" {
		s.reportAuth(context.Background(), platformaudit.Event{ActorID: session.PlatformUserID, Action: "auth.logout", ResourceType: "auth_session", Result: "SUCCESS", RiskLevel: "LOW"})
	}
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

// ConsumeLoginTransaction 原子消费未过期的登录事务，拒绝重复回调。
//
//	ConsumeLoginTransaction 读取并删除登录事务（单次消费，防重放）。
//
// 参数：ctx为调用上下文与取消边界。
//
//	state为原始登录事务随机状态。
//
// 返回值：匹配的有效持久记录；不存在、失效或存储失败返回错误。
func (s *Service) ConsumeLoginTransaction(ctx context.Context, state string) (LoginTransaction, error) {
	if s.centralClient != nil {
		return s.consumeCentralLogin(ctx, state)
	}
	var transaction LoginTransaction
	err := s.db.Where("state_hash = ?", tokenHash(state)).First(&transaction).Error
	if err != nil {
		return LoginTransaction{}, err
	}
	if time.Now().UTC().After(transaction.ExpiresAt) {
		return LoginTransaction{}, errors.New("login transaction expired")
	}
	deleted := s.db.Where("state_hash = ?", transaction.StateHash).Delete(&LoginTransaction{})
	if deleted.Error != nil {
		return LoginTransaction{}, deleted.Error
	}
	if deleted.RowsAffected != 1 {
		// 并发回调只能有一个请求删除成功，其余按 state 重放处理。
		return LoginTransaction{}, errors.New("login transaction already consumed")
	}
	return transaction, nil
}

func validateIDTokenClaims(claims idTokenClaims, expectedNonce, configuredTenant, transactionTenant string) error {
	if strings.TrimSpace(claims.Sub) == "" || claims.Sub != strings.TrimSpace(claims.Sub) {
		return errors.New("ID token subject is missing")
	}
	if expectedNonce == "" || claims.Nonce != expectedNonce {
		return errors.New("nonce does not match")
	}
	if claims.TokenUse != "id_token" {
		return errors.New("token_use must be id_token")
	}
	if claims.TenantID == "" || claims.TenantID != strings.TrimSpace(claims.TenantID) {
		return errors.New("ID token tenant is missing")
	}
	if configuredTenant != "" && claims.TenantID != configuredTenant {
		return errors.New("configured tenant does not match ID token")
	}
	if transactionTenant != "" && claims.TenantID != transactionTenant {
		return errors.New("login transaction tenant does not match ID token")
	}
	return nil
}

func validateAuthorizationBinding(value AuthorizationContext, tokenSubject, platformSubjectID, tenant, clientID, applicationCode, environmentCode string) (string, error) {
	canonicalSubjectID := strings.TrimSpace(value.SubjectID)
	if canonicalSubjectID == "" {
		canonicalSubjectID = strings.TrimSpace(value.IdentityID)
	}
	if canonicalSubjectID == "" || value.IdentityID != canonicalSubjectID || value.TenantID != tenant || value.AuthorizationRevision == 0 {
		return "", errors.New("authorization context platform subject, tenant or revision is invalid")
	}
	if tokenSubject != "" && value.Subject != tokenSubject {
		return "", errors.New("OIDC and authorization token subjects differ")
	}
	if platformSubjectID != "" && canonicalSubjectID != platformSubjectID {
		return "", errors.New("authorization context platform subject changed")
	}
	if value.ClientID != clientID || value.ApplicationCode != applicationCode || value.EnvironmentCode != environmentCode {
		return "", errors.New("authorization context client, application or environment binding mismatch")
	}
	return canonicalSubjectID, nil
}

func validateCatalogWindow(value AuthorizationContext, localVersion, localHash string) error {
	allMissing := value.CatalogVersion == "" && len(value.CompatibleCatalogVersions) == 0 && value.RoleConfigHash == "" && len(value.CompatibleRoleConfigHashes) == 0
	if allMissing {
		return nil
	}
	if localVersion == "" || localHash == "" || value.CatalogVersion == "" || value.RoleConfigHash == "" || len(value.CompatibleCatalogVersions) == 0 || len(value.CompatibleCatalogVersions) > 2 || len(value.CompatibleRoleConfigHashes) == 0 || len(value.CompatibleRoleConfigHashes) > 2 {
		return errors.New("authorization catalog compatibility window is incomplete")
	}
	if !catalogValuePresent(value.CompatibleCatalogVersions, value.CatalogVersion) || !catalogValuePresent(value.CompatibleCatalogVersions, localVersion) || !catalogValuePresent(value.CompatibleRoleConfigHashes, value.RoleConfigHash) || !catalogValuePresent(value.CompatibleRoleConfigHashes, localHash) {
		return errors.New("local authorization catalog is outside the N/N-1 compatibility window")
	}
	return nil
}

func catalogValuePresent(values []string, expected string) bool {
	seen := map[string]struct{}{}
	found := false
	for _, value := range values {
		if value == "" || value != strings.TrimSpace(value) {
			return false
		}
		if _, duplicate := seen[value]; duplicate {
			return false
		}
		seen[value] = struct{}{}
		found = found || value == expected
	}
	return found
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
