// Package importreport 提供历史分栏和受限机器报告水位查询。
package importreport

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"strings"
	"time"
)

const StatusScope = "analysis.contract-import.report-status"

var ErrInvalidMachine = errors.New("invalid report-status machine token")

type VerifierOptions struct {
	Issuer            string
	Audience          string
	PublicKeyPath     string
	ClientID          string
	TenantID          string
	CallerEnvironment string
}

type Verifier struct {
	options VerifierOptions
	key     ed25519.PublicKey
}

type applicationClaims struct {
	Issuer          string   `json:"iss"`
	Audience        string   `json:"aud"`
	Subject         string   `json:"sub"`
	OAuthClientID   string   `json:"oauth_client_id"`
	TenantID        string   `json:"tenant_id"`
	ApplicationCode string   `json:"application_code"`
	EnvironmentCode string   `json:"environment_code"`
	TokenUse        string   `json:"token_use"`
	Scope           []string `json:"scope"`
	IssuedAt        int64    `json:"iat"`
	NotBefore       int64    `json:"nbf"`
	ExpiresAt       int64    `json:"exp"`
}

// NewVerifier 构建独立用途的Ed25519平台机器验证器。
// 参数：options 绑定唯一发行者、受众、客户、租户、环境和只读公钥。
// 返回值：失败关闭的验证器；缺配置或公钥无效返回错误。
func NewVerifier(options VerifierOptions) (*Verifier, error) {
	for _, value := range []string{options.Issuer, options.Audience, options.PublicKeyPath, options.ClientID, options.TenantID, options.CallerEnvironment} {
		if strings.TrimSpace(value) == "" {
			return nil, errors.New("report-status machine trust binding incomplete")
		}
	}
	raw, err := os.ReadFile(options.PublicKeyPath)
	if err != nil {
		return nil, errors.New("report-status public key unavailable")
	}
	block, rest := pem.Decode(raw)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("report-status public key must be a single PEM block")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, errors.New("invalid report-status public key")
	}
	key, ok := parsed.(ed25519.PublicKey)
	if !ok || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("report-status key must be Ed25519")
	}
	return &Verifier{options: options, key: key}, nil
}

// Verify 校验签名及固定contract_management调用方和scope。
// 参数：ctx 为取消上下文，raw 为Bearer原始JWT。
// 返回值：可信租户；任何伪造、跨租户或跨用途令牌返回认证错误。
func (v *Verifier) Verify(ctx context.Context, raw string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	parts := strings.Split(raw, ".")
	if v == nil || len(raw) > 16384 || len(parts) != 3 {
		return "", ErrInvalidMachine
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", ErrInvalidMachine
	}
	var h struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if json.Unmarshal(header, &h) != nil || h.Alg != "EdDSA" || h.Typ != "JWT" {
		return "", ErrInvalidMachine
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(v.key, []byte(parts[0]+"."+parts[1]), signature) {
		return "", ErrInvalidMachine
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ErrInvalidMachine
	}
	var c applicationClaims
	if json.Unmarshal(payload, &c) != nil {
		return "", ErrInvalidMachine
	}
	now := time.Now().Unix()
	o := v.options
	if c.Issuer != o.Issuer ||
		c.Audience != o.Audience ||
		c.Subject != o.ClientID ||
		c.OAuthClientID == "" ||
		c.TenantID != o.TenantID ||
		c.ApplicationCode != "contract_management" ||
		c.EnvironmentCode != o.CallerEnvironment ||
		c.TokenUse != "application" ||
		len(c.Scope) != 1 ||
		c.Scope[0] != StatusScope ||
		c.IssuedAt <= 0 ||
		c.NotBefore < c.IssuedAt ||
		c.NotBefore > now+60 ||
		c.ExpiresAt <= now ||
		c.ExpiresAt <= c.NotBefore {
		return "", ErrInvalidMachine
	}
	return c.TenantID, nil
}
