package oidc

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func TestBackchannelLogoutClaims(t *testing.T) {
	now := time.Unix(1700000000, 0)
	c := logoutClaims{Subject: "u1", JTI: "j1", Issued: now.Unix() - 1, Expires: now.Unix() + 60, Audience: "data-analysis", Events: map[string]json.RawMessage{backchannelEvent: json.RawMessage(`{}`)}}
	if !validLogoutClaims(c, "data-analysis", now) {
		t.Fatal("valid claims rejected")
	}
	c.Nonce = "unexpected"
	if validLogoutClaims(c, "data-analysis", now) {
		t.Fatal("nonce-bearing logout token accepted")
	}
}

func TestBackchannelLogoutType(t *testing.T) {
	h, _ := json.Marshal(map[string]string{"typ": "logout+jwt"})
	if !validLogoutType(base64.RawURLEncoding.EncodeToString(h) + ".p.s") {
		t.Fatal("logout+jwt rejected")
	}
}
