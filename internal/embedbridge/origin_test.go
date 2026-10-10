package embedbridge

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

func TestIsolatedEmbedOrigin(t *testing.T) {
	for _, pair := range [][2]string{
		{"https://platform.example.com", "https://charts.example.net"},
		{"http://127.0.0.1:18111", "http://127.0.0.2:18131"},
		{"http://127.0.0.1:18111", "http://localhost:18131"},
	} {
		if _, err := isolatedEmbedOrigin(pair[0], pair[1]); err != nil {
			t.Fatal(err)
		}
	}
	for _, pair := range [][2]string{
		{"https://platform.example.com", "https://platform.example.com:8443"},
		{"https://platform.example.com", "https://charts.example.com"},
		{"https://platform.example.com", "http://charts.example.net"},
		{"http://127.0.0.1:18111", "http://127.0.0.1:18131"},
		{"https://platform.example.com", "https://user:secret@charts.example.net"},
		{"https://platform.example.com", "https://charts.example.net/path"},
		{"https://platform.example.com", "https://charts.example.net/?q=x"},
		{"https://platform.example.com", "https://charts.example.net/#x"},
	} {
		if _, err := isolatedEmbedOrigin(pair[0], pair[1]); err == nil {
			t.Fatal("unsafe origin accepted", pair)
		}
	}
}

func TestPlatformHostCannotServeExecutableEmbed(t *testing.T) {
	b := &Bridge{options: Options{EmbedPublicOrigin: "https://charts.example.net"}}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "https://platform.example.com/data_analysis/api/v1/embed-proxy/token", nil)
	if b.requireEmbedHost(c) || w.Code != http.StatusForbidden {
		t.Fatal("platform host allowed")
	}
}

func TestUnconfiguredEmbedOriginDoesNotPersistGrant(t *testing.T) {
	b, err := New(nil, Options{MetabaseInternalURL: "http://metabase:3000", EmbeddingSecret: "test-secret", DashboardIDs: map[string]string{"overview": "2"}})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/embed/overview", nil)
	c.Request = c.Request.WithContext(auth.WithPrincipal(c.Request.Context(), auth.Principal{TenantID: "tenant-1", UserID: "user-1", Permissions: map[string]struct{}{"dashboard.overview.view": {}}}))
	b.Issue(c, "overview")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatal(w.Code, w.Body.String())
	}
}
