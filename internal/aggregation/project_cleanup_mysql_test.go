package aggregation

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/unified-identity-auth-platform/data-analysis/internal/migration"
	"github.com/unified-identity-auth-platform/data-analysis/migrations"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Same explicit isolation guard as the publication fixture. No DSN discovery.
func TestProjectOrphanStagingRealMySQL(t *testing.T) {
	file := os.Getenv("UIP_BI_PROJECTION_TEST_DSN_FILE")
	if file == "" {
		t.Skip("explicit isolated private DSN file required")
	}
	info, e := os.Lstat(file)
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("private regular DSN file required")
	}
	raw, e := os.ReadFile(file)
	if e != nil {
		t.Fatal("read private DSN")
	}
	cfg, e := driver.ParseDSN(strings.TrimSpace(string(raw)))
	if e != nil || !strings.HasSuffix(cfg.DBName, "_test") || cfg.Net != "tcp" || !strings.HasPrefix(cfg.Addr, "127.0.0.1:") {
		t.Fatal("isolated loopback _test DSN required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, e = migration.Run(ctx, cfg.FormatDSN(), migrations.Files); e != nil {
		t.Fatal(e)
	}
	db, e := gorm.Open(mysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Discard})
	if e != nil {
		t.Fatal("open isolated DB")
	}
	pool, _ := db.DB()
	defer pool.Close()
	r := &Runner{aggDB: db.WithContext(ctx), tenantID: "synthetic-tenant"}
	cleanup := func() {
		for _, table := range []string{"project_projection_stage", "project_projection_state"} {
			if e := db.Table(table).Where("tenant_id IN ?", []string{r.tenantID, "other"}).Delete(nil).Error; e != nil {
				t.Error(e)
			}
		}
	}
	cleanup()
	defer cleanup()
	assertProjectOrphanReclamation(t, r)
}
