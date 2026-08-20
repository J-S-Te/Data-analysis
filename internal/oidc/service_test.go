package oidc

import "testing"

func TestValidateIDTokenClaimsRequiresNonceAndTenantBinding(t *testing.T) {
	valid := idTokenClaims{Sub: "subject-1", TenantID: "tenant-1", Nonce: "nonce-1", TokenUse: "id_token"}
	if err := validateIDTokenClaims(valid, "nonce-1", "tenant-1", "tenant-1"); err != nil {
		t.Fatalf("valid claims rejected: %v", err)
	}
	tests := []struct {
		name              string
		claims            idTokenClaims
		expectedNonce     string
		configuredTenant  string
		transactionTenant string
	}{
		{name: "missing nonce", claims: valid, expectedNonce: "", configuredTenant: "tenant-1", transactionTenant: "tenant-1"},
		{name: "nonce mismatch", claims: valid, expectedNonce: "nonce-2", configuredTenant: "tenant-1", transactionTenant: "tenant-1"},
		{name: "configured tenant mismatch", claims: valid, expectedNonce: "nonce-1", configuredTenant: "tenant-2", transactionTenant: "tenant-1"},
		{name: "transaction tenant mismatch", claims: valid, expectedNonce: "nonce-1", configuredTenant: "tenant-1", transactionTenant: "tenant-2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateIDTokenClaims(test.claims, test.expectedNonce, test.configuredTenant, test.transactionTenant); err == nil {
				t.Fatal("invalid claims were accepted")
			}
		})
	}
}

func TestValidateAuthorizationBinding(t *testing.T) {
	valid := AuthorizationContext{
		Subject:         "subject-1",
		IdentityID:      "identity-1",
		TenantID:        "tenant-1",
		ClientID:        "data_analysis-dev-web",
		ApplicationCode: "data_analysis",
		EnvironmentCode: "dev",
	}
	if err := validateAuthorizationBinding(valid, "subject-1", "tenant-1", "data_analysis-dev-web", "data_analysis", "dev"); err != nil {
		t.Fatalf("valid authorization binding rejected: %v", err)
	}
	tests := map[string]AuthorizationContext{
		"subject":     func() AuthorizationContext { value := valid; value.Subject = "subject-2"; return value }(),
		"tenant":      func() AuthorizationContext { value := valid; value.TenantID = "tenant-2"; return value }(),
		"client":      func() AuthorizationContext { value := valid; value.ClientID = "other-client"; return value }(),
		"application": func() AuthorizationContext { value := valid; value.ApplicationCode = "other"; return value }(),
		"environment": func() AuthorizationContext { value := valid; value.EnvironmentCode = "prod"; return value }(),
	}
	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			if err := validateAuthorizationBinding(value, "subject-1", "tenant-1", "data_analysis-dev-web", "data_analysis", "dev"); err == nil {
				t.Fatal("mismatched authorization binding was accepted")
			}
		})
	}
}
