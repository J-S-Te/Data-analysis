package oidc

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	central "github.com/unified-identity-auth-platform/data-analysis/internal/sessiongovernance"
	"golang.org/x/oauth2"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	loginTestIssuer    = "https://login-test.invalid"
	loginTestClientID  = "login-test-client"
	loginTestTenant    = "login-test-tenant"
	loginTestSubject   = "login-test-subject"
	loginTestNonce     = "login-test-nonce"
	loginTestSessionID = "login-test-sid"
)

// loginTestCentralStub 只实现登录关联需要的 Complete，其余中央协议不参与本测试。
type loginTestCentralStub struct {
	central.Client
	binding central.Binding
}

// Complete 返回预置的中央绑定，用于验证登录会话仍采用中央权威窗口。
func (c loginTestCentralStub) Complete(context.Context, string, string, string) (central.Binding, error) {
	return c.binding, nil
}

// issueLoginTestIDToken 用临时 RSA 密钥签发测试 IdP 的身份令牌，并返回只信任该公钥的验证器。
// 参数：t为测试上下文，now为本轮固定认证时间。
// 返回值：签名令牌与验证器；密钥或签名失败使测试失败。
func issueLoginTestIDToken(t *testing.T, now time.Time) (string, *oidc.IDTokenVerifier) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{
		"iss": loginTestIssuer, "aud": loginTestClientID, "sub": loginTestSubject,
		"identity_id": loginTestSubject, "tenant_id": loginTestTenant, "sid": loginTestSessionID,
		"token_use": "id_token", "nonce": loginTestNonce,
		// 带上用户名可跳过 UserInfo 在线查询，本测试不启动 fake provider。
		"name": "Login Test", "preferred_username": "login.test",
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	message, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := message.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	verifier := oidc.NewVerifier(loginTestIssuer, &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}},
		&oidc.Config{ClientID: loginTestClientID, SupportedSigningAlgs: []string{string(jose.RS256)}, Now: func() time.Time { return now }})
	return raw, verifier
}

// newLoginTestService 装配只依赖本地替身的登录服务：令牌端点和授权上下文端点均为独立测试服务器。
// 参数：t为测试上下文，idToken为令牌端点返回的身份令牌，expiresIn为秒数（0 表示不返回 expires_in）。
// 返回值：登录服务与独立 SQLite 库；装配失败使测试失败。
func newLoginTestService(t *testing.T, idToken string, expiresIn int) (*Service, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "login.sqlite")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err = db.AutoMigrate(&Session{}); err != nil {
		t.Fatalf("auto migrate session: %v", err)
	}
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := map[string]any{
			"access_token": "login-test-access", "refresh_token": "login-test-refresh",
			"token_type": "Bearer", "id_token": idToken,
		}
		if expiresIn > 0 {
			body["expires_in"] = expiresIn
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(tokenServer.Close)
	authzServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer login-test-access" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(AuthorizationContext{
			Subject: loginTestSubject, SubjectID: loginTestSubject, IdentityID: loginTestSubject,
			TenantID: loginTestTenant, ClientID: loginTestClientID,
			ApplicationCode: "data_analysis", EnvironmentCode: "dev",
			Roles: []string{"analyst"}, Permissions: []string{"dashboard.overview.view"},
			AuthorizationRevision: 7,
			DataScopes:            []DataScope{{RoleCode: "analyst", ScopeType: "TENANT", ScopeID: loginTestTenant, EnvironmentCode: "dev"}},
		})
	}))
	t.Cleanup(authzServer.Close)
	codec, err := newSecretCodec(bytes.Repeat([]byte{5}, 32))
	if err != nil {
		t.Fatalf("new codec: %v", err)
	}
	return &Service{
		db: db, codec: codec,
		tenantID: loginTestTenant, clientID: loginTestClientID,
		applicationCode: "data_analysis", environmentCode: "dev",
		sessionIdleTTL: 30 * time.Minute, sessionAbsoluteTTL: 8 * time.Hour,
		httpClient:              tokenServer.Client(),
		authorizationContextURL: authzServer.URL,
		config: oauth2.Config{
			ClientID: loginTestClientID, ClientSecret: "login-test-secret",
			Endpoint:    oauth2.Endpoint{TokenURL: tokenServer.URL, AuthStyle: oauth2.AuthStyleInHeader},
			RedirectURL: "http://localhost/data_analysis/auth/callback",
			Scopes:      []string{"openid", "profile"},
		},
	}, db
}

// TestLoginSessionAccessTokenExpiryIsAlwaysSet 回归线上 1292：
// central 关闭时 access_token_expires_at 曾保持零值并以 '0000-00-00' 落库而登录失败；
// 同时验证 central 分支仍按 binding 的权威窗口覆盖本地推导的会话期限。
// 参数：t为测试上下文。返回值：无；任一分支写入零值或中央窗口被本地值替换时失败。
func TestLoginSessionAccessTokenExpiryIsAlwaysSet(t *testing.T) {
	t.Run("central_disabled_without_expires_in", func(t *testing.T) {
		now := time.Now().UTC()
		idToken, verifier := issueLoginTestIDToken(t, now)
		service, db := newLoginTestService(t, idToken, 0)
		service.verifier = verifier

		before := time.Now().UTC()
		_, session, err := service.ExchangeAndCreateSession(context.Background(), "login-test-code", "login-test-verifier", loginTestNonce, LoginTransaction{TenantID: loginTestTenant})
		if err != nil {
			t.Fatalf("exchange without central client: %v", err)
		}
		if session.AccessTokenExpiresAt.IsZero() {
			t.Fatal("AccessTokenExpiresAt is zero when central client is disabled and IdP hides expires_in")
		}
		if !session.AccessTokenExpiresAt.Equal(session.ExpiresAt.UTC()) {
			t.Fatalf("AccessTokenExpiresAt = %v, want session absolute expiry %v", session.AccessTokenExpiresAt, session.ExpiresAt)
		}
		assertWithin(t, "session expiry", session.ExpiresAt, before.Add(8*time.Hour), time.Minute)

		var stored Session
		if err = db.Where("session_id_hash = ?", session.SessionIDHash).Take(&stored).Error; err != nil {
			t.Fatalf("load stored session: %v", err)
		}
		if stored.AccessTokenExpiresAt.IsZero() {
			t.Fatal("stored access_token_expires_at is zero")
		}
		assertWithin(t, "stored access token expiry", stored.AccessTokenExpiresAt, session.AccessTokenExpiresAt, time.Millisecond)
	})

	t.Run("central_disabled_with_expires_in", func(t *testing.T) {
		now := time.Now().UTC()
		idToken, verifier := issueLoginTestIDToken(t, now)
		service, _ := newLoginTestService(t, idToken, 300)
		service.verifier = verifier

		before := time.Now().UTC()
		_, session, err := service.ExchangeAndCreateSession(context.Background(), "login-test-code", "login-test-verifier", loginTestNonce, LoginTransaction{TenantID: loginTestTenant})
		if err != nil {
			t.Fatalf("exchange without central client: %v", err)
		}
		assertWithin(t, "access token expiry", session.AccessTokenExpiresAt, before.Add(300*time.Second), time.Minute)
		if session.AccessTokenExpiresAt.Equal(session.ExpiresAt) {
			t.Fatal("provider expires_in must win over session absolute expiry")
		}
	})

	t.Run("central_binding_overrides_without_expires_in", func(t *testing.T) {
		now := time.Now().UTC()
		idToken, verifier := issueLoginTestIDToken(t, now)
		service, db := newLoginTestService(t, idToken, 0)
		service.verifier = verifier
		centralExpiry := now.Add(4 * time.Hour).Truncate(time.Second)
		binding := central.Binding{Token: "login-test-central-binding", Context: central.Context{
			SessionID: "login-test-central-session", TenantID: loginTestTenant, SubjectID: loginTestSubject,
			ClientID: loginTestClientID, ApplicationCode: "data_analysis", EnvironmentCode: "dev",
			PolicyVersion: 1, IdleTimeoutSeconds: 1800, IdleRemainingMS: 1800000,
			AbsoluteExpiresAt: centralExpiry, AbsoluteRemainingMS: (4 * time.Hour).Milliseconds(),
		}}
		service.centralClient = loginTestCentralStub{binding: binding}
		ticket, err := service.codec.encrypt("login-test-ticket")
		if err != nil {
			t.Fatal(err)
		}

		_, session, err := service.ExchangeAndCreateSession(context.Background(), "login-test-code", "login-test-verifier", loginTestNonce,
			LoginTransaction{TenantID: loginTestTenant, CentralTicketCipher: ticket})
		if err != nil {
			t.Fatalf("exchange with central client: %v", err)
		}
		if !session.ExpiresAt.Equal(centralExpiry) {
			t.Fatalf("ExpiresAt = %v, want binding absolute %v", session.ExpiresAt, centralExpiry)
		}
		if !session.AccessTokenExpiresAt.Equal(centralExpiry) {
			t.Fatalf("AccessTokenExpiresAt = %v, want binding absolute fallback %v", session.AccessTokenExpiresAt, centralExpiry)
		}
		if len(session.CentralBindingCipher) == 0 || session.OIDCSubject != loginTestSubject || session.OIDCSessionID != loginTestSessionID {
			t.Fatal("central binding material was not persisted")
		}

		var stored Session
		if err = db.Where("session_id_hash = ?", session.SessionIDHash).Take(&stored).Error; err != nil {
			t.Fatalf("load stored session: %v", err)
		}
		if stored.AccessTokenExpiresAt.IsZero() || !stored.ExpiresAt.Equal(centralExpiry) {
			t.Fatalf("stored session lost central window: expiry=%v access=%v", stored.ExpiresAt, stored.AccessTokenExpiresAt)
		}
	})

	t.Run("central_binding_keeps_provider_expiry", func(t *testing.T) {
		now := time.Now().UTC()
		idToken, verifier := issueLoginTestIDToken(t, now)
		service, _ := newLoginTestService(t, idToken, 300)
		service.verifier = verifier
		centralExpiry := now.Add(4 * time.Hour).Truncate(time.Second)
		service.centralClient = loginTestCentralStub{binding: central.Binding{Token: "login-test-central-binding", Context: central.Context{
			SessionID: "login-test-central-session", TenantID: loginTestTenant, SubjectID: loginTestSubject,
			ClientID: loginTestClientID, ApplicationCode: "data_analysis", EnvironmentCode: "dev",
			PolicyVersion: 1, IdleTimeoutSeconds: 1800, IdleRemainingMS: 1800000,
			AbsoluteExpiresAt: centralExpiry, AbsoluteRemainingMS: (4 * time.Hour).Milliseconds(),
		}}}
		ticket, err := service.codec.encrypt("login-test-ticket")
		if err != nil {
			t.Fatal(err)
		}

		before := time.Now().UTC()
		_, session, err := service.ExchangeAndCreateSession(context.Background(), "login-test-code", "login-test-verifier", loginTestNonce,
			LoginTransaction{TenantID: loginTestTenant, CentralTicketCipher: ticket})
		if err != nil {
			t.Fatalf("exchange with central client: %v", err)
		}
		if !session.ExpiresAt.Equal(centralExpiry) {
			t.Fatalf("ExpiresAt = %v, want binding absolute %v", session.ExpiresAt, centralExpiry)
		}
		assertWithin(t, "access token expiry", session.AccessTokenExpiresAt, before.Add(300*time.Second), time.Minute)
	})
}

// assertWithin 断言两个时刻的差值在容差内，避免依赖内部 time.Now 的精确值。
func assertWithin(t *testing.T, label string, got, want time.Time, tolerance time.Duration) {
	t.Helper()
	delta := got.Sub(want)
	if delta < 0 {
		delta = -delta
	}
	if delta > tolerance {
		t.Fatalf("%s = %v, want %v (±%v)", label, got, want, tolerance)
	}
}
