package infrastructure

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/alerts/application"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// TestAlertPageReaches201stRowOnMySQL 验证 API 可翻页越过旧版
// 硬编码 200 行上限，并按业务顺序排列严重性。
func TestAlertPageReaches201stRowOnMySQL(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("DASHBOARD_ALERT_TEST_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("DASHBOARD_ALERT_TEST_MYSQL_DSN is required")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	tenant := "qa-alert-page-000000000001"
	now := time.Now().UTC()
	if err := db.Exec(`DELETE FROM alert_item WHERE tenant_id=?`, tenant).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Exec(`DELETE FROM alert_item WHERE tenant_id=?`, tenant).Error })
	seed := func(prefix, severity string, count int) {
		for i := 0; i < count; i++ {
			item := &alertRecord{ID: fmt.Sprintf("%s%023d", prefix, i), TenantID: tenant, AlertType: "CONTRACT_EXPIRY", RuleCode: "R-" + severity, Severity: severity, TargetRef: fmt.Sprintf("%s-%d", prefix, i), Title: severity + " alert", Status: "OPEN", CreatedAt: now.Format("2006-01-02 15:04:05"), UpdatedAt: now.Format("2006-01-02 15:04:05")}
			if err := db.Create(item).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	seed("H", "HIGH", 201)
	seed("M", "MEDIUM", 2)
	seed("L", "LOW", 2)
	repo := NewGORMRepository(db)
	page, err := repo.ListByTenant(context.Background(), tenant, application.ListQuery{Scope: application.AlertScope{Mode: "TENANT"}, Page: 2, PageSize: 100, Severity: "HIGH"})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 201 || len(page.Items) != 100 || page.Page != 2 || page.PageSize != 100 {
		t.Fatalf("page total=%d items=%d page=%d size=%d, want 201/100/2/100", page.Total, len(page.Items), page.Page, page.PageSize)
	}
	all, err := repo.ListByTenant(context.Background(), tenant, application.ListQuery{Scope: application.AlertScope{Mode: "TENANT"}, Page: 1, PageSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Items) != 100 || all.Items[0].Severity != "HIGH" {
		t.Fatalf("first severity=%q, want HIGH first", all.Items[0].Severity)
	}
	last, err := repo.ListByTenant(context.Background(), tenant, application.ListQuery{Scope: application.AlertScope{Mode: "TENANT"}, Page: 5, PageSize: 50})
	if err != nil || len(last.Items) != 5 || last.Total != 205 {
		t.Fatalf("last page items=%d total=%d err=%v, want 5/205", len(last.Items), last.Total, err)
	}
}
