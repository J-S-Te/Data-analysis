package migration

import (
	"crypto/sha256"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/unified-identity-auth-platform/data-analysis/migrations"
)

func TestLoadEmbeddedMigrations(t *testing.T) {
	t.Parallel()

	items, err := Load(migrations.Files)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(items) == 0 {
		t.Fatal("Load() returned no migrations")
	}
	for index, item := range items {
		wantVersion := uint64(index + 1)
		if item.Version != wantVersion {
			t.Fatalf("item[%d].Version = %d, want %d", index, item.Version, wantVersion)
		}
		if item.Checksum != sha256.Sum256([]byte(item.SQL)) {
			t.Fatalf("item[%d] checksum does not match SQL content", index)
		}
	}
}

func TestEmbeddedMigrationsCoverDashboardAPISnapshots(t *testing.T) {
	t.Parallel()

	items, err := Load(migrations.Files)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	allSQL := strings.Builder{}
	for _, item := range items {
		allSQL.WriteString(item.SQL)
		allSQL.WriteByte('\n')
	}
	for _, tableName := range []string{"api_contract_dashboard", "api_project_dashboard"} {
		if !strings.Contains(allSQL.String(), "CREATE TABLE IF NOT EXISTS "+tableName) {
			t.Errorf("embedded migrations do not create %s", tableName)
		}
	}
}

func TestEmbeddedMigrationsProtectTenantScopedAdministration(t *testing.T) {
	t.Parallel()

	items, err := Load(migrations.Files)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	var tenantMigration string
	for _, item := range items {
		if item.Version == 9 {
			tenantMigration = item.SQL
			break
		}
	}
	for _, expected := range []string{
		"ALTER TABLE sync_source", "tenant_id", "uk_sync_source_tenant",
		"ALTER TABLE sync_job", "active_key", "uk_sync_job_active_source",
		"ALTER TABLE alert_rule", "uk_alert_rule_tenant",
	} {
		if !strings.Contains(tenantMigration, expected) {
			t.Errorf("tenant migration does not contain %q", expected)
		}
	}
}

func TestLoadRejectsChangedVersionSequence(t *testing.T) {
	t.Parallel()

	source := fstest.MapFS{
		"000001_first.sql": {Data: []byte("SELECT 1;")},
		"000003_third.sql": {Data: []byte("SELECT 3;")},
	}
	_, err := Load(source)
	if err == nil || !strings.Contains(err.Error(), "expected 000002") {
		t.Fatalf("Load() error = %v, want missing-version error", err)
	}
}

func TestLoadRejectsInvalidNamesAndEmptyFiles(t *testing.T) {
	t.Parallel()

	testCases := map[string]fstest.MapFS{
		"invalid name": {
			"001_init.sql": {Data: []byte("SELECT 1;")},
		},
		"empty migration": {
			"000001_init.sql": {Data: []byte(" \n\t")},
		},
	}
	for name, source := range testCases {
		source := source
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := Load(source); err == nil {
				t.Fatal("Load() error = nil, want validation failure")
			}
		})
	}
}

func TestOpenDatabaseRequiresDSN(t *testing.T) {
	t.Parallel()

	if _, err := openDatabase(" "); err == nil || !strings.Contains(err.Error(), "DASHBOARD_MYSQL_DSN") {
		t.Fatalf("openDatabase() error = %v, want required DSN error", err)
	}
}
