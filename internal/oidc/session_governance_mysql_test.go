package oidc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	central "github.com/unified-identity-auth-platform/data-analysis/internal/sessiongovernance"
	"golang.org/x/oauth2"

	driver "github.com/go-sql-driver/mysql"
	"github.com/unified-identity-auth-platform/data-analysis/internal/migration"
	"github.com/unified-identity-auth-platform/data-analysis/migrations"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// centralMySQLDatabase 只在显式独立测试库应用真实迁移，不寻找业务配置。
// 参数：t为测试上下文；DSN仅来自SESSION_GOVERNANCE_TEST_DSN。
// 返回值：真实OIDC存储；无专用DSN则跳过，不符合回环隔离边界则失败。
func centralMySQLDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("SESSION_GOVERNANCE_TEST_DSN")
	if dsn == "" {
		t.Skip("isolated SESSION_GOVERNANCE_TEST_DSN is not configured")
	}
	config, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("invalid isolated DSN")
	}
	host, portText, err := net.SplitHostPort(config.Addr)
	port, portErr := strconv.Atoi(portText)
	if err != nil || portErr != nil || config.Net != "tcp" || host != "127.0.0.1" ||
		port < 32768 || config.DBName != "qa_session_analysis_20261101" {
		t.Fatal("refusing non-isolated session DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	if _, err := migration.Run(ctx, dsn, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if _, err := migration.Run(ctx, dsn, migrations.Files); err != nil {
		t.Fatal("migration rerun:", err)
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return db
}

// refreshFixtureID 签发独立测试IdP的真实RSA身份令牌供刷新路径完整验签。
// 参数：t为测试上下文，now为本轮固定认证时间。
// 返回值：签名身份令牌及只信任该临时公钥的验证器。
func refreshFixtureID(t *testing.T, now time.Time) (string, *oidc.IDTokenVerifier) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"iss": "https://isolated-issuer.test", "aud": "qa-client",
		"sub": "qa-identity", "identity_id": "qa-identity", "tenant_id": "qa-session", "sid": "qa-sid",
		"token_use": "id_token", "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix()})
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
	verifier := oidc.NewVerifier("https://isolated-issuer.test", &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}},
		&oidc.Config{ClientID: "qa-client", SupportedSigningAlgs: []string{"RS256"}, Now: func() time.Time { return now }})
	return raw, verifier
}

type analysisFixedCentral struct {
	central.Client
	state central.Context
}

// State 提供本轮唯一原始周期，不模拟任何业务授权成功。
// 参数：请求上下文及服务器绑定秘密。
// 返回值：固定原始截止的中央窗口。
func (c analysisFixedCentral) State(context.Context, string) (central.Context, error) {
	return c.state, nil
}

// TestCentralSessionRotationCheckpointMySQL 验证数析在单次refresh后权限503仍保留新令牌。
// 参数：t为隔离MySQL上下文；IdP为临时HTTP/RSA替身，真实Keycloak另验。
// 返回值：无；重复轮换、错误退出、权限伪更新或原始截止变化时失败。
func TestCentralSessionRotationCheckpointMySQL(t *testing.T) {
	db := centralMySQLDatabase(t)
	ctx, now := context.Background(), time.Now().UTC().Truncate(time.Millisecond)
	rawID, verifier := refreshFixtureID(t, now)
	var refreshes atomic.Int32
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/token" {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":"temporarily_unavailable"}`))
			return
		}
		if refreshes.Add(1) != 1 || r.FormValue("refresh_token") != "isolated-old-refresh" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "isolated-access", "refresh_token": "isolated-new-refresh",
			"token_type": "Bearer", "expires_in": 300, "id_token": rawID})
	}))
	t.Cleanup(issuer.Close)
	codec, err := newSecretCodec(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	access, err := codec.encrypt("isolated-old-access")
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := codec.encrypt("isolated-old-refresh")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := codec.encrypt("isolated-server-binding")
	if err != nil {
		t.Fatal(err)
	}
	cookie := "isolated-analysis-rotation-" + now.Format("150405.000")
	session := Session{SessionIDHash: tokenHash(cookie), TenantID: "qa-session", PlatformUserID: "qa-identity",
		OIDCSubject: "qa-identity", OIDCSessionID: "qa-sid", Username: "qa-user", DisplayName: "isolated QA",
		RolesJSON: "[]", PermissionsJSON: "[]", DataScopesJSON: "[]", RoleConfigHash: "qa-catalog", AuthzRevision: 1,
		AccessTokenCipher: access, RefreshTokenCipher: refresh, AccessTokenExpiresAt: now.Add(30 * time.Second),
		CentralBindingCipher: binding, CentralSessionID: "qa-central", ExpiresAt: now.Add(8 * time.Hour),
		AuthorizationCheckedAt: now, CreatedAt: now, LastSeenAt: now}
	if err := db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Where("session_id_hash=?", session.SessionIDHash).Delete(&Session{}).Error })
	service := &Service{db: db, codec: codec, verifier: verifier, tenantID: "qa-session", clientID: "qa-client", applicationCode: "data_analysis", environmentCode: "test",
		httpClient: issuer.Client(), authorizationContextURL: issuer.URL + "/authorization", checkEvery: time.Minute,
		config: oauth2.Config{ClientID: "qa-client", Endpoint: oauth2.Endpoint{TokenURL: issuer.URL + "/token", AuthStyle: oauth2.AuthStyleInHeader}},
		centralClient: analysisFixedCentral{state: central.Context{SessionID: "qa-central", TenantID: "qa-session", SubjectID: "qa-identity", ClientID: "qa-client",
			ApplicationCode: "data_analysis", EnvironmentCode: "test", PolicyVersion: 1, IdleTimeoutSeconds: 1800, IdleRemainingMS: 1800000,
			AbsoluteRemainingMS: 28800000, AbsoluteExpiresAt: session.ExpiresAt}}}
	var group sync.WaitGroup
	for range 12 {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := service.authenticateCentral(ctx, cookie); !errors.Is(err, ErrSessionUnavailable) {
				t.Errorf("outage must stay unavailable: %v", err)
			}
		}()
	}
	group.Wait()
	var stored Session
	if err := db.Where("session_id_hash=?", session.SessionIDHash).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	rotated, err := codec.decrypt(stored.RefreshTokenCipher)
	if err != nil || rotated != "isolated-new-refresh" || refreshes.Load() != 1 || stored.RevokedAt != nil {
		t.Fatal("verified rotation was lost, replayed or revoked")
	}
	if stored.AuthzRevision != 1 || !stored.AuthorizationCheckedAt.Before(now.Add(-time.Minute)) || !stored.ExpiresAt.Equal(session.ExpiresAt) {
		t.Fatal("outage bypassed authorization revalidation or reset original expiry")
	}
}
