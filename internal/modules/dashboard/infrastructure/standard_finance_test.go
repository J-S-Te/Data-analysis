package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// TestStandardFinanceUsesPublishedNamespace 验证显式来源、真实零值、范围拒绝与存储故障边界。
// 参数：t 为测试上下文。
// 返回值：无；内存夹具不冒充真实MySQL验收。
func TestStandardFinanceUsesPublishedNamespace(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := db.DB()
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	for _, ddl := range []string{
		`CREATE TABLE business_dashboard_snapshot(tenant_id TEXT, section TEXT, payload TEXT)`,
		`CREATE TABLE standard_finance_source(tenant_id TEXT,source_code TEXT,batch_id TEXT,version INTEGER,published_at DATETIME)`,
		`CREATE TABLE standard_finance_batch(tenant_id TEXT,source_code TEXT,batch_id TEXT,version INTEGER,snapshot_json TEXT)`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatal(err)
		}
	}
	legacy, _ := json.Marshal(domain.BusinessSnapshot{Available: true, TenantID: "tenant", SnapshotAt: "2026-11-04T00:00:00Z", Metrics: map[string]any{"as_of_date": "2026-10-31"}})
	db.Exec("INSERT INTO business_dashboard_snapshot VALUES(?,?,?)", "tenant", "finance", string(legacy))
	repo := NewGORMRepositoryWithLocation(db, time.UTC).WithFinanceSource("standard")
	service := application.NewService(repo)
	principal := auth.Principal{TenantID: "tenant", UserID: "user", ScopeMode: "TENANT"}
	ctx := auth.WithPrincipal(context.Background(), principal)
	empty, err := service.Business(ctx, "tenant", "finance")
	if err != nil || empty.Available {
		t.Fatalf("unpublished mixed with legacy: %+v %v", empty, err)
	}
	zero, _ := json.Marshal(domain.BusinessSnapshot{Available: true, TenantID: "tenant", SnapshotAt: "2026-11-04T00:00:00Z", Metrics: map[string]any{"currencies": []any{}, "as_of_date": "2026-11-04"}})
	db.Exec("INSERT INTO standard_finance_source VALUES(?,?,?,?,?)", "tenant", "standard-finance", "batch", 1, time.Now())
	db.Exec("INSERT INTO standard_finance_batch VALUES(?,?,?,?,?)", "tenant", "standard-finance", "batch", 1, string(zero))
	published, err := service.Business(ctx, "tenant", "finance")
	if err != nil || !published.Available || published.Metrics["as_of_date"] != "2026-11-04" {
		t.Fatalf("standard zero %+v %v", published, err)
	}
	repo.WithFinanceSource("integration")
	integrated, err := service.Business(ctx, "tenant", "finance")
	if err != nil || integrated.Metrics["as_of_date"] != "2026-10-31" {
		t.Fatalf("legacy changed: %+v %v", integrated, err)
	}
	repo.WithFinanceSource("standard")
	for _, mode := range []string{"ORG", "TEAM", "SELF", "NONE", ""} {
		principal.ScopeMode = mode
		denied := auth.WithPrincipal(context.Background(), principal)
		if _, err := service.Business(denied, "tenant", "finance"); !errors.Is(err, application.ErrBusinessScopeUnavailable) {
			t.Fatalf("mode %s leaked: %v", mode, err)
		}
	}
	db.Exec("DROP TABLE standard_finance_batch")
	if _, err := service.Business(ctx, "tenant", "finance"); err == nil {
		t.Fatal("source failure hidden as no data")
	}
}
