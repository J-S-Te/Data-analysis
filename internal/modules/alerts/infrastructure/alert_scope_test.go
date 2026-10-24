package infrastructure

import (
	"context"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/alerts/application"
)

func alertScopeTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS bi_audit_outbox (
		id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, event_id TEXT NOT NULL,
		actor_id TEXT, action TEXT NOT NULL, resource_type TEXT NOT NULL, resource_id TEXT NOT NULL,
		login_ip BLOB, client_ip BLOB, ip_source TEXT NOT NULL DEFAULT 'UNKNOWN',
		result TEXT NOT NULL DEFAULT 'SUCCESS', delivery_status TEXT NOT NULL DEFAULT 'pending',
		attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at DATETIME NOT NULL, locked_at DATETIME,
		delivered_at DATETIME, last_error TEXT, created_at DATETIME NOT NULL)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&alertRecord{}); err != nil {
		t.Fatal(err)
	}
	return db
}

// 预警列表、统计和状态变更都必须遵守同一个组织/团队边界；空/未知范围 失败时安全拒绝。
func TestAlertScopeFiltersListSummaryAndStatusWrites(t *testing.T) {
	db := alertScopeTestDB(t)
	rows := []alertRecord{
		{ID: "a", TenantID: "tenant-1", AlertType: "CONTRACT_EXPIRY", RuleCode: "R", Severity: "HIGH", OwnerOrgID: "org-a", TargetRef: "c-a", Title: "A", Status: "OPEN"},
		{ID: "b", TenantID: "tenant-1", AlertType: "CONTRACT_EXPIRY", RuleCode: "R", Severity: "HIGH", OwnerOrgID: "org-b", TargetRef: "c-b", Title: "B", Status: "OPEN"},
		{ID: "c", TenantID: "tenant-1", AlertType: "CONTRACT_EXPIRY", RuleCode: "R", Severity: "HIGH", OwnerOrgID: "", TargetRef: "c-c", Title: "C", Status: "OPEN"},
		{ID: "d", TenantID: "tenant-1", AlertType: "CONTRACT_EXPIRY", RuleCode: "R", Severity: "LOW", OwnerOrgID: "org-a", TargetRef: "c-d", Title: "D", Status: "ACK"},
		{ID: "e", TenantID: "tenant-2", AlertType: "CONTRACT_EXPIRY", RuleCode: "R", Severity: "HIGH", OwnerOrgID: "org-a", TargetRef: "c-e", Title: "E", Status: "OPEN"},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	repository := NewGORMRepository(db)
	ctx := context.Background()
	orgScope := application.AlertScope{Mode: "ORG", OrgUnitIDs: []string{"org-a"}}

	page, err := repository.ListByTenant(ctx, "tenant-1", application.ListQuery{Page: 1, PageSize: 50, Scope: orgScope})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || len(page.Items) != 2 || page.Items[0].ID != "a" || page.Items[1].ID != "d" {
		t.Fatalf("org list = %+v", page)
	}
	summary, err := repository.SummaryByTenant(ctx, "tenant-1", orgScope)
	if err != nil || summary.Total != 2 || summary.Open != 1 || summary.Ack != 1 {
		t.Fatalf("org summary = %+v err=%v", summary, err)
	}
	teamPage, err := repository.ListByTenant(ctx, "tenant-1", application.ListQuery{Page: 1, PageSize: 50, Scope: application.AlertScope{Mode: "TEAM", OrgUnitIDs: []string{"org-a"}}})
	if err != nil || teamPage.Total != 2 {
		t.Fatalf("team list = %+v err=%v", teamPage, err)
	}
	tenantPage, err := repository.ListByTenant(ctx, "tenant-1", application.ListQuery{Page: 1, PageSize: 50, Scope: application.AlertScope{Mode: "TENANT"}})
	if err != nil || tenantPage.Total != 4 {
		t.Fatalf("tenant list = %+v err=%v", tenantPage, err)
	}
	nonePage, err := repository.ListByTenant(ctx, "tenant-1", application.ListQuery{Page: 1, PageSize: 50, Scope: application.AlertScope{Mode: "NONE"}})
	if err != nil || nonePage.Total != 0 {
		t.Fatalf("none list = %+v err=%v", nonePage, err)
	}
	unknownPage, err := repository.ListByTenant(ctx, "tenant-1", application.ListQuery{Page: 1, PageSize: 50})
	if err != nil || unknownPage.Total != 0 {
		t.Fatalf("empty mode must fail closed: %+v err=%v", unknownPage, err)
	}
	now := time.Now().UTC()
	updated, err := repository.UpdateStatus(ctx, "b", "tenant-1", "CLOSED", orgScope, now, &now)
	if err != nil || updated {
		t.Fatalf("cross-org update = %v err=%v", updated, err)
	}
	var b alertRecord
	if err := db.First(&b, "id = ?", "b").Error; err != nil || b.Status != "OPEN" {
		t.Fatalf("cross-org alert changed: %+v err=%v", b, err)
	}
	updated, err = repository.UpdateStatus(ctx, "a", "tenant-1", "CLOSED", orgScope, now, &now)
	if err != nil || !updated {
		t.Fatalf("in-scope update = %v err=%v", updated, err)
	}
	var a alertRecord
	if err := db.First(&a, "id = ?", "a").Error; err != nil || a.Status != "CLOSED" {
		t.Fatalf("in-scope alert not changed: %+v err=%v", a, err)
	}
	var outbox int64
	if err := db.Table("bi_audit_outbox").Where("tenant_id = ? AND resource_id = ?", "tenant-1", "a").Count(&outbox).Error; err != nil || outbox != 1 {
		t.Fatalf("audit outbox rows = %d err=%v", outbox, err)
	}
}
