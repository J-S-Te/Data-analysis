package infrastructure

import (
	"context"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/alerts/application"
)

func TestSummaryGroupsOnlyAuthenticatedTenant(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	// AUD-IP-2026：状态变更与审计同事务，测试库需有同名 outbox 表。
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
	rows := []alertRecord{
		{ID: "1", TenantID: "own", Status: "OPEN", Severity: "HIGH", AlertType: "SLA"},
		{ID: "2", TenantID: "own", Status: "ACK", Severity: "HIGH", AlertType: "SLA"},
		{ID: "3", TenantID: "own", Status: "CLOSED", Severity: "LOW", AlertType: "RESOURCE"},
		{ID: "4", TenantID: "other", Status: "OPEN", Severity: "HIGH", AlertType: "SLA"},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	repository := NewGORMRepository(db)
	summary, err := repository.SummaryByTenant(context.Background(), "own", application.AlertScope{Mode: "TENANT"})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Total != 3 || summary.Open != 1 || summary.Ack != 1 || summary.Closed != 1 ||
		summary.BySeverity["HIGH"] != 2 || summary.BySeverity["LOW"] != 1 ||
		summary.ByType["SLA"] != 2 || summary.ByType["RESOURCE"] != 1 {
		t.Fatalf("incorrect scoped aggregation: %+v", summary)
	}
	empty, err := repository.SummaryByTenant(context.Background(), "empty", application.AlertScope{Mode: "TENANT"})
	if err != nil || empty.Total != 0 || len(empty.ByType) != 0 || len(empty.BySeverity) != 0 {
		t.Fatalf("empty summary: %+v error=%v", empty, err)
	}
	if err := db.Migrator().DropTable(&alertRecord{}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.SummaryByTenant(context.Background(), "own", application.AlertScope{Mode: "TENANT"}); err == nil {
		t.Fatal("database failure must not silently return an empty summary")
	}
}
