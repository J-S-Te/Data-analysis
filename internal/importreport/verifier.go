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
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

const StatusScope = "analysis.contract-import.report-status"

var ErrInvalidMachine = errors.New("invalid report-status machine token")

type VerifierOptions struct {
	Issuer            string
	Audience          string
	PublicKeyPath     string
	ClientID          string
	OAuthClientID     string
	TenantID          string
	CallerEnvironment string
	// RequireApplicationID / RequireEnvironmentID 显式声明机器令牌必须绑定平台
	// 应用与环境。令牌只证明“某个客户端在某个调用方环境”发起的读取，缺任一
	// 标识就无法做跨应用/跨环境隔离，因此构造期即要求为 true，不允许隐式放宽。
	RequireApplicationID bool
	RequireEnvironmentID bool
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
	ApplicationID   string   `json:"application_id"`
	ApplicationCode string   `json:"application_code"`
	EnvironmentID   string   `json:"environment_id"`
	EnvironmentCode string   `json:"environment_code"`
	TokenUse        string   `json:"token_use"`
	Scope           []string `json:"scope"`
	IssuedAt        int64    `json:"iat"`
	NotBefore       int64    `json:"nbf"`
	ExpiresAt       int64    `json:"exp"`
}

// jwtHeaderFields / applicationClaimFields 是平台应用令牌当前实际签发的字段白名单。
// 验签只接受这些字段：未知字段、重复键、null 与尾随 JSON 都按非法令牌拒绝，
// 避免不同 JSON 解析器对同一载荷给出不同解释。
var (
	jwtHeaderFields        = map[string]struct{}{"alg": {}, "typ": {}}
	applicationClaimFields = map[string]struct{}{
		"iss": {}, "aud": {}, "sub": {}, "oauth_client_id": {}, "tenant_id": {},
		"application_id": {}, "application_code": {}, "environment_id": {}, "environment_code": {},
		"token_use": {}, "scope": {}, "iat": {}, "nbf": {}, "exp": {},
	}
)

// decodeStrictObject 解析单个 JSON 对象：拒绝未知字段、重复键、null 值、
// 无效 UTF-8 与对象之后的任何尾随内容，并在此之后按目标类型解码。
// 参数：raw 为待解析字节，fields 为允许的字段集合，out 为类型化目标。
// 返回值：任何歧义返回 ErrInvalidMachine，绝不返回部分解析结果。
func decodeStrictObject(raw []byte, fields map[string]struct{}, out any) error {
	if !utf8.Valid(raw) {
		return ErrInvalidMachine
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return ErrInvalidMachine
	}
	seen := make(map[string]struct{}, len(fields))
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return ErrInvalidMachine
		}
		key, ok := token.(string)
		if !ok {
			return ErrInvalidMachine
		}
		if _, allowed := fields[key]; !allowed {
			return ErrInvalidMachine
		}
		if _, duplicate := seen[key]; duplicate {
			return ErrInvalidMachine
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return ErrInvalidMachine
		}
		if len(value) == 0 || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return ErrInvalidMachine
		}
		seen[key] = struct{}{}
	}
	if _, err := decoder.Token(); err != nil { // 对象结束的 '}'
		return ErrInvalidMachine
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ErrInvalidMachine
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return ErrInvalidMachine
	}
	return nil
}

// NewVerifier 构建独立用途的Ed25519平台机器验证器。
// 参数：options 绑定唯一发行者、受众、OAuth 客户端、应用、租户、环境和只读公钥。
// 返回值：失败关闭的验证器；缺配置、未要求应用/环境绑定或公钥无效返回错误。
func NewVerifier(options VerifierOptions) (*Verifier, error) {
	for _, value := range []string{options.Issuer, options.Audience, options.PublicKeyPath, options.ClientID, options.OAuthClientID, options.TenantID, options.CallerEnvironment} {
		if strings.TrimSpace(value) == "" {
			return nil, errors.New("report-status machine trust binding incomplete")
		}
	}
	// 应用/环境绑定是机器读取审计的最小上下文；允许关闭等于允许匿名调用方，
	// 因此不接受“未要求”的配置，构造期即失败。
	if !options.RequireApplicationID || !options.RequireEnvironmentID {
		return nil, errors.New("report-status machine trust binding incomplete")
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
	if decodeStrictObject(header, jwtHeaderFields, &h) != nil || h.Alg != "EdDSA" || h.Typ != "JWT" {
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
	if decodeStrictObject(payload, applicationClaimFields, &c) != nil {
		return "", ErrInvalidMachine
	}
	now := time.Now().Unix()
	o := v.options
	if c.Issuer != o.Issuer ||
		c.Audience != o.Audience ||
		c.Subject != o.ClientID ||
		c.OAuthClientID != o.OAuthClientID ||
		c.ApplicationID == "" ||
		c.EnvironmentID == "" ||
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
