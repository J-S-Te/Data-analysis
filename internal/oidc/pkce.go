package oidc

import (
	"crypto/rand"
	"encoding/base64"

	"golang.org/x/oauth2"
)

// oauth2Verifier 生成 PKCE S256 verifier（43-128 字符，无填充 base64url）。
func oauth2Verifier() string {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	return base64.RawURLEncoding.EncodeToString(raw)
}

var _ = oauth2.S256ChallengeOption // 保持 oauth2 依赖显式（PKCE 校验在 oauth2.VerifierOption）
