package infrastructure

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/admin/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/admin/domain"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestRuleLifecyclePersistenceAndHistoryProtection(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })
	for _, ddl := range []string{
		`CREATE TABLE alert_rule (id TEXT PRIMARY KEY, tenant_id TEXT, rule_code TEXT, name TEXT, source_fct TEXT, severity TEXT, enabled BOOLEAN, threshold_json TEXT, updated_by TEXT, created_at DATETIME, updated_at DATETIME, UNIQUE(tenant_id,rule_code))`,
		`CREATE TABLE alert_rule_version (id TEXT PRIMARY KEY, tenant_id TEXT, rule_id TEXT, version INTEGER, rule_snapshot TEXT, changed_by TEXT, created_at DATETIME, UNIQUE(tenant_id,rule_id,version))`,
		`CREATE TABLE alert_item (tenant_id TEXT, rule_code TEXT)`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatal(err)
		}
	}
	repo := NewGORMRepository(db)
	svc := application.NewService(repo)
	ctx := context.Background()
	rows, err := svc.ListAlertRules(ctx, "tenant-a")
	if err != nil || len(rows) != 0 {
		t.Fatalf("unexpected defaults: %v %v", rows, err)
	}
	threshold := `{"days":30}`
	input := domain.AlertRule{RuleCode: "CONTRACT_EXPIRY", Name: "到期提醒", SourceFCT: "dim_contract", Severity: "MEDIUM", Enabled: false, ThresholdJSON: &threshold}
	rows, err = svc.ReplaceAlertRules(ctx, "tenant-a", "actor", []domain.AlertRule{input})
	if err != nil || len(rows) != 1 {
		t.Fatalf("create: %v %v", rows, err)
	}
	created := rows[0]
	if len(created.ID) != 26 || created.Version != 1 || created.Enabled {
		t.Fatalf("bad created rule: %+v", created)
	}
	rows, err = svc.ReplaceAlertRules(ctx, "tenant-b", "actor", []domain.AlertRule{input})
	if err != nil {
		t.Fatal(err)
	}
	otherID := rows[0].ID
	created.Name = "修订提醒"
	created.Enabled = true
	rows, err = svc.ReplaceAlertRules(ctx, "tenant-a", "actor", []domain.AlertRule{created})
	if err != nil {
		t.Fatal(err)
	}
	updated := rows[0]
	if updated.ID != created.ID || updated.Version != 2 || !updated.Enabled || !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("identity/version not preserved: %+v", updated)
	}
	if _, err = svc.ReplaceAlertRules(ctx, "tenant-a", "actor", []domain.AlertRule{created}); !errors.Is(err, application.ErrRuleVersionConflict) {
		t.Fatalf("stale save accepted: %v", err)
	}
	forged := updated
	forged.ID = otherID
	if _, err = svc.ReplaceAlertRules(ctx, "tenant-a", "actor", []domain.AlertRule{forged}); !errors.Is(err, application.ErrRuleVersionConflict) {
		t.Fatalf("foreign id accepted: %v", err)
	}
	updated.Enabled = false
	rows, err = svc.ReplaceAlertRules(ctx, "tenant-a", "actor", []domain.AlertRule{updated})
	if err != nil || rows[0].Enabled || rows[0].Version != 3 {
		t.Fatalf("disable: %v %v", rows, err)
	}
	if err = svc.DeleteAlertRule(ctx, "tenant-b", created.ID); !errors.Is(err, application.ErrRuleNotFound) {
		t.Fatalf("foreign deletion: %v", err)
	}
	if err = db.Exec(`INSERT INTO alert_item (tenant_id,rule_code) VALUES (?,?)`, "tenant-a", "CONTRACT_EXPIRY").Error; err != nil {
		t.Fatal(err)
	}
	if err = svc.DeleteAlertRule(ctx, "tenant-a", created.ID); !errors.Is(err, application.ErrRuleHasHistory) {
		t.Fatalf("history deletion: %v", err)
	}
	if err = svc.DeleteAlertRule(ctx, "tenant-b", otherID); err != nil {
		t.Fatal(err)
	}
	rows, err = svc.ListAlertRules(ctx, "tenant-b")
	if err != nil || len(rows) != 0 {
		t.Fatalf("deleted rule resurrected: %v %v", rows, err)
	}
	var snapshots int64
	if err = db.Table("alert_rule_version").Where("tenant_id = ? AND rule_id = ?", "tenant-b", otherID).Count(&snapshots).Error; err != nil || snapshots != 1 {
		t.Fatalf("history lost: %d %v", snapshots, err)
	}
	var original domain.AlertRule
	if err = db.Table("alert_rule_version").Where("tenant_id = ? AND rule_id = ? AND version = 1", "tenant-a", created.ID).Select("created_at").Scan(&original).Error; err != nil {
		t.Fatal(err)
	}
	if original.CreatedAt.IsZero() || original.CreatedAt.After(time.Now().Add(time.Minute)) {
		t.Fatal("invalid snapshot time")
	}
}
