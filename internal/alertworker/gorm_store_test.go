package alertworker

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func ruleTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.Exec(`CREATE TABLE alert_rule (tenant_id TEXT, rule_code TEXT, severity TEXT, threshold_json BLOB, enabled BOOLEAN)`).Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func TestListEnabledRulesIsReadOnlyAndTenantScoped(t *testing.T) {
	db := ruleTestDB(t)
	store := NewGormStore(db, "tenant-a")
	for run := 0; run < 2; run++ {
		rules, err := store.ListEnabledRules(context.Background())
		if err != nil || len(rules) != 0 {
			t.Fatalf("empty rules = %v, error = %v", rules, err)
		}
	}
	if err := db.Exec(`INSERT INTO alert_rule VALUES
('tenant-a', 'CONTRACT_EXPIRY', 'HIGH', '{"days":30}', 1),
('tenant-a', 'DISABLED', 'HIGH', '{"days":30}', 0),
('tenant-b', 'OTHER', 'HIGH', '{"days":30}', 1)`).Error; err != nil {
		t.Fatal(err)
	}
	rules, err := store.ListEnabledRules(context.Background())
	if err != nil || len(rules) != 1 || rules[0].Code != RuleContractExpiry {
		t.Fatalf("enabled rules = %v, error = %v", rules, err)
	}
	if err := db.Exec("DELETE FROM alert_rule WHERE tenant_id = ? AND rule_code = ?", "tenant-a", RuleContractExpiry).Error; err != nil {
		t.Fatal(err)
	}
	for run := 0; run < 2; run++ {
		rules, err := store.ListEnabledRules(context.Background())
		if err != nil || len(rules) != 0 {
			t.Fatalf("deleted rule reappeared: rules = %v, error = %v", rules, err)
		}
	}
	var count int64
	if err := db.Table("alert_rule").Count(&count).Error; err != nil || count != 2 {
		t.Fatalf("rule count = %d, error = %v; want untouched disabled and other tenant rules", count, err)
	}
}

func dryRunMySQL(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(mysql.New(mysql.Config{
		DSN: "unused:unused@tcp(127.0.0.1:1)/unused", SkipInitializeWithVersion: true,
	}), &gorm.Config{DryRun: true, DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func TestContractExpiryQueryBindsWorkerTenant(t *testing.T) {
	db := dryRunMySQL(t)
	var sql string
	var vars []any
	if err := db.Callback().Query().After("gorm:query").Register("capture_candidate_sql", func(tx *gorm.DB) {
		sql = tx.Statement.SQL.String()
		vars = append([]any(nil), tx.Statement.Vars...)
	}); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	_, err := NewGormStore(db, "tenant-a").FindContractExpiryCandidates(context.Background(), at, 21)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "tenant_id = ?") {
		t.Fatalf("candidate SQL lacks tenant predicate: %s", sql)
	}
	want := []any{"tenant-a", at, at, 21, "COMPLETED", "ARCHIVED", "TERMINATED"}
	if !reflect.DeepEqual(vars, want) {
		t.Fatalf("candidate SQL vars = %#v, want %#v", vars, want)
	}
}

func TestContractExpiryQueryRejectsContaminatedCandidates(t *testing.T) {
	db := dryRunMySQL(t)
	if err := db.Callback().Query().After("gorm:query").Register("contaminated_candidates", func(tx *gorm.DB) {
		rows := tx.Statement.Dest.(*[]ContractExpiryCandidate)
		*rows = []ContractExpiryCandidate{{TenantID: "tenant-b", TargetRef: "contract-b"}}
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := NewGormStore(db, "tenant-a").FindContractExpiryCandidates(context.Background(), time.Now(), 30)
	if err == nil || rows != nil {
		t.Fatalf("contaminated candidates = %v, error = %v; want rejection", rows, err)
	}
}

func TestGormStoreRejectsEmptyTenantBeforeDatabaseAccess(t *testing.T) {
	for _, tenant := range []string{"", "  "} {
		store := NewGormStore(nil, tenant)
		if _, err := store.ListEnabledRules(context.Background()); err == nil {
			t.Fatal("ListEnabledRules accepted empty tenant")
		}
		if _, err := store.FindContractExpiryCandidates(context.Background(), time.Now(), 30); err == nil {
			t.Fatal("FindContractExpiryCandidates accepted empty tenant")
		}
		if err := store.UpsertAlerts(context.Background(), nil); err == nil {
			t.Fatal("UpsertAlerts accepted empty tenant")
		}
	}
}

func TestUpsertAlertsRejectsCrossTenantBeforeDatabaseAccess(t *testing.T) {
	err := NewGormStore(nil, "tenant-a").UpsertAlerts(context.Background(), []Alert{
		{TenantID: "tenant-a", RuleCode: RuleContractExpiry},
		{TenantID: "tenant-b", RuleCode: RuleContractExpiry},
	})
	if err == nil {
		t.Fatal("UpsertAlerts accepted cross tenant batch")
	}
}

func TestUpsertAlertsSkipsDisabledOrDeletedRule(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "deleted"}[enabled], func(t *testing.T) {
			db := ruleTestDB(t)
			if err := db.Exec("INSERT INTO alert_rule (tenant_id, rule_code, enabled) VALUES (?, ?, ?)", "tenant-a", RuleContractExpiry, enabled).Error; err != nil {
				t.Fatal(err)
			}
			if enabled {
				if err := db.Exec("DELETE FROM alert_rule").Error; err != nil {
					t.Fatal(err)
				}
			}
			// 没有 alert_item 表；任何写入都会失败，缺失/停用规则必须直接跳过。
			if err := NewGormStore(db, "tenant-a").UpsertAlerts(context.Background(), []Alert{{
				TenantID: "tenant-a", RuleCode: RuleContractExpiry, TargetRef: "contract-a",
			}}); err != nil {
				t.Fatalf("inactive rule attempted alert write: %v", err)
			}
		})
	}
}

func TestUpsertAlertsLocksTenantRuleBeforeWriting(t *testing.T) {
	connection, err := ruleTestDB(t).DB()
	if err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: connection, SkipInitializeWithVersion: true}),
		&gorm.Config{DryRun: true, DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	locked := false
	written := false
	if err := db.Callback().Query().After("gorm:query").Register("capture_rule_lock", func(tx *gorm.DB) {
		sql := tx.Statement.SQL.String()
		if !strings.Contains(sql, "FOR UPDATE") || !strings.Contains(sql, "tenant_id = ? AND rule_code = ?") {
			t.Errorf("rule lock SQL = %s", sql)
		}
		if !reflect.DeepEqual(tx.Statement.Vars, []any{"tenant-a", RuleContractExpiry, 1}) {
			t.Errorf("rule lock vars = %#v", tx.Statement.Vars)
		}
		locked = true
		tx.Statement.Dest.(*struct{ Enabled bool }).Enabled = true
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Raw().After("gorm:raw").Register("capture_alert_write", func(tx *gorm.DB) {
		if !locked {
			t.Error("alert write preceded rule lock")
		}
		if !strings.Contains(tx.Statement.SQL.String(), "INSERT INTO alert_item") || tx.Statement.Vars[1] != "tenant-a" {
			t.Errorf("unexpected alert SQL = %s, vars = %#v", tx.Statement.SQL.String(), tx.Statement.Vars)
		}
		written = true
	}); err != nil {
		t.Fatal(err)
	}
	if err := NewGormStore(db, "tenant-a").UpsertAlerts(context.Background(), []Alert{{
		TenantID: "tenant-a", RuleCode: RuleContractExpiry, TargetRef: "contract-a",
	}}); err != nil {
		t.Fatal(err)
	}
	if !locked || !written {
		t.Fatalf("rule locked = %v, alert written = %v", locked, written)
	}
}
