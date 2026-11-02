package importreport

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// strictTestVerifier 生成临时 Ed25519 信任绑定的验证器。
// 参数：t 为测试上下文。
// 返回值：验证器与测试私钥。
func strictTestVerifier(t *testing.T) (*Verifier, ed25519.PrivateKey) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "public.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(VerifierOptions{Issuer: "platform", Audience: "analysis", PublicKeyPath: path, ClientID: "contract-reader", OAuthClientID: "x", TenantID: "t", CallerEnvironment: "test", RequireApplicationID: true, RequireEnvironmentID: true})
	if err != nil {
		t.Fatal(err)
	}
	return verifier, key
}

// signRawToken 用原始 header/payload 文本签名，允许构造非严格 JSON 负例。
// 参数：key 为测试私钥，header/payload 为原始 JSON 文本。
// 返回值：紧凑 JWT。
func signRawToken(key ed25519.PrivateKey, header, payload string) string {
	signingInput := base64.RawURLEncoding.EncodeToString([]byte(header)) + "." + base64.RawURLEncoding.EncodeToString([]byte(payload))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(signingInput)))
}

// TestVerifyRejectsAmbiguousJWTJSON 验证验签只接受严格 JSON：
// 未知字段、重复键、null、类型错配与尾随内容都不能被不同解析器解释成有效令牌。
func TestVerifyRejectsAmbiguousJWTJSON(t *testing.T) {
	verifier, key := strictTestVerifier(t)
	now := time.Now().Unix()
	validPayload := fmt.Sprintf(`{"iss":"platform","aud":"analysis","sub":"contract-reader","oauth_client_id":"x","tenant_id":"t","application_id":"app-1","application_code":"contract_management","environment_id":"env-1","environment_code":"test","token_use":"application","scope":["analysis.contract-import.report-status"],"iat":%d,"nbf":%d,"exp":%d}`, now-1, now-1, now+60)
	const validHeader = `{"alg":"EdDSA","typ":"JWT"}`

	tenant, err := verifier.Verify(context.Background(), signRawToken(key, validHeader, validPayload))
	if err != nil || tenant != "t" {
		t.Fatalf("baseline token rejected: tenant=%q err=%v", tenant, err)
	}

	cases := []struct {
		name    string
		header  string
		payload string
	}{
		{"duplicate claim key", validHeader, strings.Replace(validPayload, `"tenant_id":"t"`, `"tenant_id":"t","tenant_id":"t"`, 1)},
		{"unknown claim field", validHeader, strings.Replace(validPayload, `"tenant_id":"t"`, `"tenant_id":"t","extra":true`, 1)},
		{"trailing claim value", validHeader, validPayload + `{}`},
		{"null claim value", validHeader, strings.Replace(validPayload, `"tenant_id":"t"`, `"tenant_id":null`, 1)},
		{"wrong claim type", validHeader, strings.Replace(validPayload, `"scope":["analysis.contract-import.report-status"]`, `"scope":"analysis.contract-import.report-status"`, 1)},
		{"invalid utf8 claim", validHeader, strings.Replace(validPayload, `"iss":"platform"`, "\"iss\":\"plat\xfform\"", 1)},
		{"duplicate header key", `{"alg":"EdDSA","alg":"EdDSA","typ":"JWT"}`, validPayload},
		{"unknown header field", `{"alg":"EdDSA","typ":"JWT","kid":"k1"}`, validPayload},
		{"trailing header value", validHeader + `[]`, validPayload},
		{"null header value", `{"alg":"EdDSA","typ":null}`, validPayload},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tenant, err := verifier.Verify(context.Background(), signRawToken(key, tc.header, tc.payload))
			if !errors.Is(err, ErrInvalidMachine) || tenant != "" {
				t.Fatalf("ambiguous token accepted: tenant=%q err=%v", tenant, err)
			}
		})
	}
}

// TestNewVerifierRequiresOAuthClientAndApplicationEnvironmentBinding 验证机器令牌的
// 信任绑定不完整时构造期即失败（BIZ-XAPI-13）：缺 OAuth 客户端或关闭应用/环境
// 绑定都会让端点无法证明“哪个应用在哪个环境调用了什么”。
func TestNewVerifierRequiresOAuthClientAndApplicationEnvironmentBinding(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "public.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	complete := VerifierOptions{Issuer: "platform", Audience: "analysis", PublicKeyPath: path, ClientID: "contract-reader", OAuthClientID: "x", TenantID: "t", CallerEnvironment: "test", RequireApplicationID: true, RequireEnvironmentID: true}
	if _, err := NewVerifier(complete); err != nil {
		t.Fatalf("complete binding rejected: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*VerifierOptions)
	}{
		{"missing oauth client id", func(o *VerifierOptions) { o.OAuthClientID = "" }},
		{"blank oauth client id", func(o *VerifierOptions) { o.OAuthClientID = "   " }},
		{"application binding disabled", func(o *VerifierOptions) { o.RequireApplicationID = false }},
		{"environment binding disabled", func(o *VerifierOptions) { o.RequireEnvironmentID = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := complete
			tc.mutate(&options)
			if _, err := NewVerifier(options); err == nil {
				t.Fatal("incomplete machine trust binding must be rejected")
			}
		})
	}
}

// TestVerifyBindsOAuthClientApplicationAndEnvironment 验证验签把令牌绑定到具体
// OAuth 客户端、应用与环境：跨客户端、缺应用标识、缺环境标识都按非法令牌拒绝，
// 且不削弱既有的 iss/aud/租户/scope/时间窗校验。
func TestVerifyBindsOAuthClientApplicationAndEnvironment(t *testing.T) {
	verifier, key := strictTestVerifier(t)
	now := time.Now().Unix()
	const validHeader = `{"alg":"EdDSA","typ":"JWT"}`
	validPayload := fmt.Sprintf(`{"iss":"platform","aud":"analysis","sub":"contract-reader","oauth_client_id":"x","tenant_id":"t","application_id":"app-1","application_code":"contract_management","environment_id":"env-1","environment_code":"test","token_use":"application","scope":["analysis.contract-import.report-status"],"iat":%d,"nbf":%d,"exp":%d}`, now-1, now-1, now+60)
	if tenant, err := verifier.Verify(context.Background(), signRawToken(key, validHeader, validPayload)); err != nil || tenant != "t" {
		t.Fatalf("baseline token rejected: tenant=%q err=%v", tenant, err)
	}
	for _, tc := range []struct{ name, payload string }{
		{"other oauth client", strings.Replace(validPayload, `"oauth_client_id":"x"`, `"oauth_client_id":"y"`, 1)},
		{"missing application id", strings.Replace(validPayload, `"application_id":"app-1",`, ``, 1)},
		{"blank application id", strings.Replace(validPayload, `"application_id":"app-1"`, `"application_id":""`, 1)},
		{"missing environment id", strings.Replace(validPayload, `"environment_id":"env-1",`, ``, 1)},
		{"blank environment id", strings.Replace(validPayload, `"environment_id":"env-1"`, `"environment_id":""`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tenant, err := verifier.Verify(context.Background(), signRawToken(key, validHeader, tc.payload))
			if !errors.Is(err, ErrInvalidMachine) || tenant != "" {
				t.Fatalf("tenant=%q err=%v, want ErrInvalidMachine", tenant, err)
			}
		})
	}
}
