package http

import (
	"context"
	"fmt"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dictionary/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

func invoke(t *testing.T, fn gin.HandlerFunc, p *auth.Principal, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("PUT", "/dictionary", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	if p != nil {
		c.Request = c.Request.WithContext(auth.WithPrincipal(context.Background(), *p))
	}
	fn(c)
	return w
}
func TestDictionaryPermissionChecks(t *testing.T) {
	h := NewHandler(application.NewCatalogService())
	admin := auth.Principal{UserID: "actor", TenantID: "tenant", Roles: []string{"admin"}}
	w := invoke(t, h.Put, &admin, "[]")
	if w.Code != 403 {
		t.Fatalf("role bypass %d", w.Code)
	}
	w = invoke(t, h.Get, nil, "")
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	reader := auth.Principal{UserID: "actor", TenantID: "tenant", Permissions: map[string]struct{}{"dictionary.view": {}}}
	w = invoke(t, h.Get, &reader, "")
	if w.Code != 500 || !strings.Contains(w.Body.String(), "METRIC_STORAGE_FAILED") {
		t.Fatalf("storage response %d %s", w.Code, w.Body.String())
	}
	w = invoke(t, h.Create, &reader, "{}")
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
}
func TestToggleRequiresBoolean(t *testing.T) {
	h := NewHandler(application.NewCatalogService())
	manager := auth.Principal{UserID: "actor", TenantID: "tenant", Permissions: map[string]struct{}{"dictionary.manage": {}}}
	w := invoke(t, h.SetEnabled, &manager, `{"version":1}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "METRIC_PAYLOAD_INVALID") {
		t.Fatalf("invalid toggle %d %s", w.Code, w.Body.String())
	}
}
func TestStorageErrorDoesNotLeakSQL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	respondError(c, sqlFailure{})
	if w.Code != 500 || strings.Contains(w.Body.String(), "password") {
		t.Fatalf("unsafe response %s", w.Body.String())
	}
}

func TestMutationRequiresVersion(t *testing.T) {
	h := NewHandler(application.NewCatalogService())
	p := auth.Principal{UserID: "actor", TenantID: "tenant", Permissions: map[string]struct{}{"dictionary.manage": {}}}
	for _, tt := range []struct {
		fn   gin.HandlerFunc
		body string
	}{{h.Update, `{"code":"custom"}`}, {h.SetEnabled, `{"enabled":true}`}, {h.Delete, `{}`}} {
		w := invoke(t, tt.fn, &p, tt.body)
		if w.Code != 409 || !strings.Contains(w.Body.String(), "METRIC_VERSION_CONFLICT") {
			t.Fatalf("version missing %d %s", w.Code, w.Body.String())
		}
	}
}

func TestHTTPPrincipalOwnsTenantAndActor(t *testing.T) {
	db, e := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	if e != nil {
		t.Fatal(e)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	for _, ddl := range []string{
		`CREATE TABLE metric_definition (id TEXT PRIMARY KEY,tenant_id TEXT,code TEXT,name TEXT,dashboard TEXT,definition TEXT,formula TEXT,source TEXT,period TEXT,status TEXT,version INTEGER,origin TEXT,enabled INTEGER,updated_by TEXT,created_at DATETIME,updated_at DATETIME,deleted_at DATETIME,UNIQUE(tenant_id,code))`,
		`CREATE TABLE metric_definition_version (id TEXT PRIMARY KEY,tenant_id TEXT,metric_id TEXT,version INTEGER,metric_snapshot TEXT,operation TEXT,actor_id TEXT,created_at DATETIME,UNIQUE(tenant_id,metric_id,version))`,
	} {
		if e = db.Exec(ddl).Error; e != nil {
			t.Fatal(e)
		}
	}
	h := NewHandler(application.NewCatalogService(db))
	p := auth.Principal{UserID: "principal-actor", TenantID: "tenant-a", Permissions: map[string]struct{}{"dictionary.manage": {}, "dictionary.view": {}}}
	body := `{"code":"custom.a","name":"tenant a name","dashboard":"dashboard","definition":"definition","formula":"description","source":"source","period":"month","status":"待确认","tenant_id":"tenant-b","actor_id":"forged"}`
	w := invoke(t, h.Create, &p, body)
	if w.Code != 200 {
		t.Fatalf("create %d %s", w.Code, w.Body.String())
	}
	var row struct {
		TenantID  string
		UpdatedBy string
	}
	if e = db.Table("metric_definition").Take(&row).Error; e != nil {
		t.Fatal(e)
	}
	if row.TenantID != "tenant-a" || row.UpdatedBy != "principal-actor" {
		t.Fatalf("untrusted identity accepted %+v", row)
	}
	other := p
	other.TenantID = "tenant-b"
	w = invoke(t, h.Get, &other, "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "tenant a name") {
		t.Fatalf("tenant leak %d %s", w.Code, w.Body.String())
	}
	if e = db.Migrator().DropTable("metric_definition"); e != nil {
		t.Fatal(e)
	}
	w = invoke(t, h.Get, &p, "")
	if w.Code != 500 || strings.Contains(w.Body.String(), "no such table") {
		t.Fatalf("SQL exposed %d %s", w.Code, w.Body.String())
	}
}

type sqlFailure struct{}

func (sqlFailure) Error() string { return "mysql password credential connection denied" }
