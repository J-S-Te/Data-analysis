package embedbridge

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRewriteEmbedDocumentUsesTokenResourcePrefix(t *testing.T) {
	input := []byte(`<html><head><base href="/mb/"><meta name="base-href" content="/mb/"><meta name="uri" content="/mb/embed/dashboard/signed"></head></html>`)
	got := string(rewriteEmbedDocument(input, "/data_analysis/api/v1/embed-proxy/raw-token/", "/mb"))
	wantParts := []string{
		`<base href="/data_analysis/api/v1/embed-proxy/raw-token/">`,
		`<meta name="base-href" content="/data_analysis/api/v1/embed-proxy/raw-token/">`,
		`<meta name="uri" content="/embed/dashboard/signed">`,
	}
	for _, want := range wantParts {
		if !contains(got, want) {
			t.Fatalf("rewritten document missing %q: %s", want, got)
		}
	}
}

func TestResourcePrefixIncludesConfiguredPathPrefix(t *testing.T) {
	bridge := &Bridge{options: Options{PathPrefix: "/data_analysis/"}}
	if got, want := bridge.resourcePrefix("token"), "/data_analysis/api/v1/embed-proxy/token/"; got != want {
		t.Fatalf("resource prefix = %q, want %q", got, want)
	}
}

func TestWithoutFrameAncestorsPreservesOtherCSPDirectives(t *testing.T) {
	input := "default-src 'none'; frame-ancestors 'none'; script-src 'self';"
	if got, want := withoutFrameAncestors(input), "default-src 'none'; script-src 'self';"; got != want {
		t.Fatalf("CSP = %q, want %q", got, want)
	}
}

func TestSignedEmbedURLCarriesTenantScope(t *testing.T) {
	bridge := &Bridge{options: Options{EmbeddingSecret: "test-secret"}}
	signed, err := bridge.signedEmbedURL("42", map[string]interface{}{
		"tenant_id":  "tenant-1",
		"scope_mode": "TENANT",
	}, time.Unix(2_000_000_000, 0))
	if err != nil {
		t.Fatalf("signedEmbedURL() error = %v", err)
	}
	parts := strings.Split(signed.token, ".")
	if len(parts) != 2 {
		t.Fatalf("signed token parts = %d, want 2", len(parts))
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var payload struct {
		Params map[string]interface{} `json:"params"`
	}
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		t.Fatalf("decode JSON payload: %v", err)
	}
	if got := payload.Params["tenant_id"]; got != "tenant-1" {
		t.Fatalf("tenant_id = %#v, want tenant-1", got)
	}
}

func TestAllowedMetabaseResourceRejectsManagementAndTraversal(t *testing.T) {
	tests := []struct {
		resource string
		allowed  bool
	}{
		{resource: "/embed/dashboard/signed-token", allowed: true},
		{resource: "/api/embed/dashboard/signed-token/query", allowed: true},
		{resource: "/app/dist/app.js", allowed: true},
		{resource: "/api/user/current", allowed: false},
		{resource: "/api/setup/admin_checklist", allowed: false},
		{resource: "/api/embed/dashboard/../user/current", allowed: false},
		{resource: "/api/embed/dashboard/%2e%2e/user/current", allowed: false},
	}
	for _, test := range tests {
		_, allowed := allowedMetabaseResource(test.resource)
		if allowed != test.allowed {
			t.Errorf("allowedMetabaseResource(%q) allowed = %t, want %t", test.resource, allowed, test.allowed)
		}
	}
}

func TestStripSensitiveProxyHeaders(t *testing.T) {
	header := http.Header{
		"Authorization":    {"Bearer platform-token"},
		"Cookie":           {"data_analysis_session=secret"},
		"X-Da-Tenant-Id":   {"tenant-2"},
		"X-Forwarded-User": {"admin"},
		"Accept":           {"application/json"},
	}
	stripSensitiveProxyHeaders(header)
	for _, name := range []string{"Authorization", "Cookie", "X-Da-Tenant-Id", "X-Forwarded-User"} {
		if header.Get(name) != "" {
			t.Errorf("sensitive header %s was not removed", name)
		}
	}
	if got := header.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept = %q, want application/json", got)
	}
}

func TestBridgeDefaultTokenTTLIsShort(t *testing.T) {
	bridge, err := New(nil, Options{MetabaseInternalURL: "http://metabase:3000", EmbeddingSecret: "test-secret"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if bridge.tokenTTL != 2*time.Minute {
		t.Fatalf("default tokenTTL = %v, want 2m", bridge.tokenTTL)
	}
}

func TestBridgeHonorsExplicitTokenTTL(t *testing.T) {
	bridge, err := New(nil, Options{
		MetabaseInternalURL: "http://metabase:3000", EmbeddingSecret: "test-secret", TokenTTL: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if bridge.tokenTTL != 30*time.Second {
		t.Fatalf("tokenTTL = %v, want 30s", bridge.tokenTTL)
	}
}

func contains(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
