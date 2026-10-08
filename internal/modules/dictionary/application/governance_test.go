package application

import (
	"context"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/unified-identity-auth-platform/data-analysis/internal/migration"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dictionary/domain"
	"github.com/unified-identity-auth-platform/data-analysis/migrations"
	"gorm.io/driver/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, e := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if e != nil {
		t.Fatal(e)
	}
	if e = db.AutoMigrate(&metricRow{}, &versionRow{}, &referenceRow{}); e != nil {
		t.Fatal(e)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { sqlDB.Close() })
	return db
}
func expectCode(t *testing.T, e error, code string) {
	t.Helper()
	typed, ok := e.(*Error)
	if !ok || typed.Code != code {
		t.Fatalf("expected %s, got %v", code, e)
	}
}
func lifecycle(t *testing.T, db *gorm.DB, tenant string) {
	t.Helper()
	ctx := context.Background()
	s := NewCatalogService(db)
	actor := "actor"
	m := domain.Metric{Code: "custom.kpi", Name: "指标", Dashboard: "合同看板", Definition: "口径", Formula: "文字描述", Source: "业务事实", Period: "月", Status: "待确认", Enabled: true, Origin: "BUILTIN", CalculationBinding: "forged"}
	created, e := s.Create(ctx, tenant, actor, m)
	if e != nil {
		t.Fatal(e)
	}
	if len(created.ID) != 26 || created.Version != 1 || created.Enabled || created.Origin != "CUSTOM" || created.CanEnable || created.CalculationBinding != "" {
		t.Fatalf("bad created %+v", created)
	}
	_, e = s.SetEnabled(ctx, tenant, actor, m.Code, true, 1)
	expectCode(t, e, "CALCULATION_BINDING_REQUIRED")
	c, e := s.ListForTenant(ctx, tenant)
	if e != nil || len(c.Metrics) != 23 {
		t.Fatalf("catalog %d %v", len(c.Metrics), e)
	}
	other, e := s.ListForTenant(ctx, tenant+"x")
	if e != nil || len(other.Metrics) != 22 {
		t.Fatalf("tenant leak %v", e)
	}
	created.Name = "变更"
	updated, e := s.Update(ctx, tenant, actor, m.Code, created)
	if e != nil || updated.Version != 2 || updated.ID != created.ID {
		t.Fatalf("update %+v %v", updated, e)
	}
	_, e = s.Update(ctx, tenant, actor, m.Code, created)
	expectCode(t, e, "METRIC_VERSION_CONFLICT")
	history, e := s.History(ctx, tenant, m.Code)
	if e != nil || len(history) != 2 || history[1].Metric.Name != "指标" {
		t.Fatalf("history %+v %v", history, e)
	}
	if e = db.Create(&referenceRow{ID: "reference", TenantID: tenant, MetricCode: m.Code, ReferenceType: "report", ReferenceID: "report-1"}).Error; e != nil {
		t.Fatal(e)
	}
	e = s.Delete(ctx, tenant, actor, m.Code, 2)
	expectCode(t, e, "METRIC_REFERENCED")
	if e = db.Where("tenant_id = ? AND id = ?", tenant, "reference").Delete(&referenceRow{}).Error; e != nil {
		t.Fatal(e)
	}
	if e = s.Delete(ctx, tenant, actor, m.Code, 2); e != nil {
		t.Fatal(e)
	}
	history, e = s.History(ctx, tenant, m.Code)
	if e != nil || len(history) != 3 || history[0].Operation != "DELETE" || history[0].Metric.Version != 3 {
		t.Fatalf("deleted history %+v %v", history, e)
	}
	_, e = s.Create(ctx, tenant, actor, m)
	expectCode(t, e, "METRIC_CODE_CONFLICT")
	c, e = s.ListForTenant(ctx, tenant)
	if e != nil || len(c.Metrics) != 22 {
		t.Fatal("deleted metric revived", e)
	}
	b, _ := builtin("2.1")
	if !b.CanEnable || !b.Enabled {
		t.Fatal("native binding missing")
	}
	b.Formula = "不同口径"
	over, e := s.Update(ctx, tenant, actor, b.Code, b)
	if e != nil || over.Enabled || over.CanEnable || over.Version != 2 {
		t.Fatalf("override %+v %v", over, e)
	}
	_, e = s.SetEnabled(ctx, tenant, actor, b.Code, true, 2)
	expectCode(t, e, "CALCULATION_BINDING_REQUIRED")
	e = s.Delete(ctx, tenant, actor, b.Code, 2)
	expectCode(t, e, "METRIC_BUILTIN_PROTECTED")
	history, e = s.History(ctx, tenant, b.Code)
	if e != nil || len(history) != 2 {
		t.Fatalf("builtin history %v %v", history, e)
	}
	if e = s.SaveTenantMetrics(ctx, tenant, actor, []domain.Metric{over}); e != nil {
		t.Fatal(e)
	}
	c, e = s.ListForTenant(ctx, tenant)
	if e != nil || len(c.Metrics) != 22 {
		t.Fatal("bulk changed unrelated metrics", e)
	}
}
func TestMetricLifecycleSQLite(t *testing.T) { lifecycle(t, testDB(t), "tenant") }
func TestStorageFailureNeverFallsBack(t *testing.T) {
	db := testDB(t)
	if e := db.Migrator().DropTable(&metricRow{}); e != nil {
		t.Fatal(e)
	}
	if _, e := NewCatalogService(db).ListForTenant(context.Background(), "tenant"); e == nil {
		t.Fatal("storage failure hidden")
	}
	if _, e := NewCatalogService().ListForTenant(context.Background(), "tenant"); e == nil {
		t.Fatal("missing storage hidden")
	}
}

func TestBulkRollbackAndReadOnlyFields(t *testing.T) {
	db := testDB(t)
	s := NewCatalogService(db)
	ctx := context.Background()
	m := domain.Metric{Code: "custom", Name: "名称", Dashboard: "看板", Definition: "口径", Formula: "公式描述", Source: "来源", Period: "月", Status: "待确认"}
	bad := m
	bad.Code = "bad"
	bad.Name = ""
	if e := s.SaveTenantMetrics(ctx, "tenant", "actor", []domain.Metric{m, bad}); e == nil {
		t.Fatal("invalid bulk accepted")
	}
	c, e := s.ListForTenant(ctx, "tenant")
	if e != nil || len(c.Metrics) != 22 {
		t.Fatal("bulk partially committed", e)
	}
	_, e = s.Create(ctx, "tenant", "", m)
	expectCode(t, e, "METRIC_VALIDATION_FAILED")
	created, e := s.Create(ctx, "tenant", "actor", m)
	if e != nil {
		t.Fatal(e)
	}
	created.Origin = "BUILTIN"
	created.Enabled = true
	created.CalculationBinding = "forged"
	created.CanEnable = true
	created.ID = "forged"
	updated, e := s.Update(ctx, "tenant", "actor", created.Code, created)
	if e != nil || updated.Origin != "CUSTOM" || updated.Enabled || updated.CanEnable || updated.ID == "forged" {
		t.Fatalf("readonly corruption %+v %v", updated, e)
	}
	updated.Code = "renamed"
	_, e = s.Update(ctx, "tenant", "actor", "custom", updated)
	expectCode(t, e, "METRIC_VALIDATION_FAILED")
}

func TestBindingRequiresConfirmedStatus(t *testing.T) {
	db := testDB(t)
	s := NewCatalogService(db)
	ctx := context.Background()
	b, _ := builtin("2.1")
	b.Status = "待确认"
	m, e := s.Update(ctx, "tenant", "actor", b.Code, b)
	if e != nil || m.Enabled || m.CanEnable {
		t.Fatalf("unconfirmed binding %+v %v", m, e)
	}
	_, e = s.SetEnabled(ctx, "tenant", "actor", m.Code, true, m.Version)
	expectCode(t, e, "CALCULATION_BINDING_REQUIRED")
	m.Status = "已确认"
	m, e = s.Update(ctx, "tenant", "actor", m.Code, m)
	if e != nil || !m.CanEnable || m.Enabled {
		t.Fatalf("confirm %+v %v", m, e)
	}
	m, e = s.SetEnabled(ctx, "tenant", "actor", m.Code, true, m.Version)
	if e != nil || !m.Enabled {
		t.Fatal("cannot enable confirmed native binding", e)
	}
	history, e := s.History(ctx, "tenant", m.Code)
	if e != nil || history[0].Operation != "ENABLE" {
		t.Fatal(history, e)
	}
}
func TestMetricLifecycleMySQL(t *testing.T) {
	dsn := os.Getenv("DA_METRIC_TEST_DSN")
	if dsn == "" {
		t.Skip("DA_METRIC_TEST_DSN dedicated MySQL fixture is not configured")
	}
	cfg, e := mysqlDriver.ParseDSN(dsn)
	if e != nil {
		t.Fatal("invalid fixture DSN")
	}
	host, _, e := net.SplitHostPort(cfg.Addr)
	if e != nil || cfg.Net != "tcp" || cfg.DBName != "metric_dictionary_test" || (host != "127.0.0.1" && host != "::1" && host != "localhost") {
		t.Fatal("fixture must use loopback TCP and metric_dictionary_test database")
	}
	if _, e = migration.Run(context.Background(), dsn, migrations.Files); e != nil {
		t.Fatal(e)
	}
	cfg.ParseTime = true
	db, e := gorm.Open(mysql.Open(cfg.FormatDSN()), &gorm.Config{})
	if e != nil {
		t.Fatal(e)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	tenant := fmt.Sprintf("test%d", time.Now().UnixNano())
	lifecycle(t, db, tenant)
}
