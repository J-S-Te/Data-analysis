package migration

import (
	"strings"
	"testing"

	"github.com/unified-identity-auth-platform/data-analysis/migrations"
)

// TestEmbeddedMigrationsAddSessionIdleActivity 验证会话空闲化迁移：
// 只允许给 da_oidc_sessions 增加可空的 last_interactive_at 并回填存量行，不得破坏其他数据。
func TestEmbeddedMigrationsAddSessionIdleActivity(t *testing.T) {
	t.Parallel()

	items, err := Load(migrations.Files)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	var idleMigration string
	for _, item := range items {
		if item.Name == "session_idle" {
			idleMigration = item.SQL
			break
		}
	}
	if idleMigration == "" {
		t.Fatal("embedded migrations do not contain a session_idle migration")
	}
	for _, fragment := range []string{
		"ALTER TABLE da_oidc_sessions",
		"ADD COLUMN last_interactive_at DATETIME(3) NULL",
		"UPDATE da_oidc_sessions",
		"SET last_interactive_at = created_at",
	} {
		if !strings.Contains(idleMigration, fragment) {
			t.Errorf("session idle migration missing %q", fragment)
		}
	}
	for _, fragment := range []string{"DROP TABLE", "TRUNCATE", "DELETE FROM"} {
		if strings.Contains(idleMigration, fragment) {
			t.Errorf("session idle migration destroys data: %s", fragment)
		}
	}
}
