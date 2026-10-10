package embedbridge

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
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
	if len(parts) != 3 {
		t.Fatalf("signed token parts = %d, want standard JWT 3", len(parts))
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || string(header) != `{"alg":"HS256","typ":"JWT"}` {
		t.Fatal("invalid JWT algorithm/header")
	}
	mac := hmac.New(sha256.New, []byte("test-secret"))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(signature, mac.Sum(nil)) {
		t.Fatal("JWT signature does not cover header and payload")
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var payload struct {
		Params   map[string]interface{} `json:"params"`
		Resource map[string]interface{} `json:"resource"`
		Expires  int64                  `json:"exp"`
	}
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		t.Fatalf("decode JSON payload: %v", err)
	}
	if got := payload.Params["tenant_id"]; got != "tenant-1" {
		t.Fatalf("tenant_id = %#v, want tenant-1", got)
	}
	if payload.Resource["dashboard"] != float64(42) || payload.Expires != 2_000_000_000 {
		t.Fatal("dashboard must be numeric and expiry must remain absolute")
	}
}

func TestMetabaseDashboardResourceRejectsUnconfiguredAndMalformedIDs(t *testing.T) {
	for _, value := range []string{"", " ", " 42", "42 ", "0", "-1", "../42", "arbitrary", "9223372036854775808"} {
		if _, err := metabaseDashboardResource(value); err == nil {
			t.Errorf("invalid dashboard accepted: %q", value)
		}
	}
	const entity = "abcdefghijklmnopqrstu"
	if got, err := metabaseDashboardResource(entity); err != nil || got != entity {
		t.Fatal("valid stable entity ID rejected")
	}
}

func TestIssueUnconfiguredDashboardDoesNotPersistGrant(t *testing.T) {
	// A nil database deliberately proves failure occurs before token persistence.
	bridge, err := New(nil, Options{MetabaseInternalURL: "http://metabase:3000", EmbeddingSecret: "test-secret"})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/embed/overview", nil)
	c.Request = c.Request.WithContext(auth.WithPrincipal(c.Request.Context(), auth.Principal{
		TenantID: "tenant-1", UserID: "user-1", Permissions: map[string]struct{}{"dashboard.overview.view": {}},
	}))
	bridge.Issue(c, "overview")
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "EMBED_DASHBOARD_UNCONFIGURED") {
		t.Fatalf("unexpected unconfigured response: %d %s", w.Code, w.Body.String())
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
