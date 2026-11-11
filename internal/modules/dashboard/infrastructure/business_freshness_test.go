package infrastructure

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// businessFreshnessDB 建立业务快照及同步状态的隔离内存夹具。
// 参数：t 为测试上下文。
// 返回值：独立数据库连接；初始化失败终止用例。
func businessFreshnessDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	for _, ddl := range []string{
		"CREATE TABLE business_dashboard_snapshot(tenant_id TEXT,section TEXT,payload TEXT)",
		"CREATE TABLE sync_source(id TEXT,tenant_id TEXT,subsystem_code TEXT,enabled BOOLEAN,last_status TEXT,last_success_at DATETIME,last_error TEXT,updated_at DATETIME)",
		"CREATE TABLE standard_finance_source(tenant_id TEXT,source_code TEXT,batch_id TEXT,version INTEGER,published_at DATETIME)",
		"CREATE TABLE standard_finance_batch(tenant_id TEXT,source_code TEXT,batch_id TEXT,version INTEGER,snapshot_json TEXT)",
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// putBusinessFreshnessSnapshot 写入当前测试的真实持久化快照载荷。
// 参数：t 为测试上下文，db 为夹具，section 为分区，value 为载荷。
// 返回值：无；写入失败终止用例。
func putBusinessFreshnessSnapshot(t *testing.T, db *gorm.DB, section string, value domain.BusinessSnapshot) {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Exec("INSERT INTO business_dashboard_snapshot VALUES('t',?,?)", section, string(payload)).Error; err != nil {
		t.Fatal(err)
	}
}

// TestBusinessFreshnessUsesSectionSuccessAndFailure 验证共享源更新不冒充本节成功时间。
// 参数：t 为测试上下文。
// 返回值：无；失败未标旧或借用别节时间时失败。
func TestBusinessFreshnessUsesSectionSuccessAndFailure(t *testing.T) {
	db := businessFreshnessDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	sectionAt := now.Add(-2 * time.Hour).Format(time.RFC3339)
	putBusinessFreshnessSnapshot(t, db, "report", domain.BusinessSnapshot{Available: true, TenantID: "t", SnapshotAt: sectionAt, SourceCode: "project_management", SourceUpdatedAt: now.Add(-3 * time.Hour).Format(time.RFC3339), Metrics: map[string]any{"active_reports": 3}})
	if err := db.Exec("INSERT INTO sync_source VALUES('s','t','project_management',1,'FAILED',?,'upstream unavailable',?)", now.Add(-time.Hour), now).Error; err != nil {
		t.Fatal(err)
	}
	got, err := NewGORMRepositoryWithLocation(db, time.UTC).LatestBusiness(context.Background(), "t", "report")
	if err != nil || !got.Available || !got.Freshness.Stale || got.Freshness.SyncStatus != "FAILED" || got.Freshness.SyncedAt != sectionAt || got.Metrics["active_reports"] != float64(3) {
		t.Fatalf("snapshot=%+v err=%v", got, err)
	}
	if got.Freshness.SourceUpdatedAt == "" || !strings.Contains(got.Freshness.StaleReason, "失败") {
		t.Fatalf("freshness=%+v", got.Freshness)
	}
	if err := db.Exec("UPDATE sync_source SET last_status='SUCCESS',last_error='' ").Error; err != nil {
		t.Fatal(err)
	}
	recent, err := NewGORMRepositoryWithLocation(db, time.UTC).LatestBusiness(context.Background(), "t", "report")
	if err != nil || recent.Freshness.Stale || recent.Freshness.SyncedAt != sectionAt {
		t.Fatalf("recent=%+v err=%v", recent, err)
	}
	disabled, err := NewGORMRepositoryWithLocation(db, time.UTC).WithIntegrationSyncEnabled(false).LatestBusiness(context.Background(), "t", "report")
	if err != nil || !disabled.Available || !disabled.Freshness.Stale || disabled.Freshness.SyncedAt != sectionAt {
		t.Fatalf("disabled=%+v err=%v", disabled, err)
	}
	if err := db.Exec("UPDATE sync_source SET enabled=0").Error; err != nil {
		t.Fatal(err)
	}
	stopped, err := NewGORMRepositoryWithLocation(db, time.UTC).LatestBusiness(context.Background(), "t", "report")
	if err != nil || !stopped.Freshness.Stale || !strings.Contains(stopped.Freshness.StaleReason, "停用") {
		t.Fatalf("stopped=%+v err=%v", stopped, err)
	}
}

// TestBusinessFreshnessUnknownLegacyAndSourceBoundary 验证缺源、旧载荷与来源绑定。
// 参数：t 为测试上下文。
// 返回值：无；未知来源伪装新鲜或跨来源回退时失败。
func TestBusinessFreshnessUnknownLegacyAndSourceBoundary(t *testing.T) {
	db := businessFreshnessDB(t)
	repository := NewGORMRepositoryWithLocation(db, time.UTC)
	missing, err := repository.LatestBusiness(context.Background(), "t", "report")
	if err != nil || missing.Available || missing.Freshness.SyncStatus != "UNKNOWN" || !missing.Freshness.Stale {
		t.Fatalf("missing=%+v err=%v", missing, err)
	}
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second).Format(time.RFC3339)
	putBusinessFreshnessSnapshot(t, db, "report", domain.BusinessSnapshot{Available: true, TenantID: "t", SnapshotAt: at, SourceCode: "project_management", Metrics: map[string]any{}})
	unknown, err := repository.LatestBusiness(context.Background(), "t", "report")
	if err != nil || !unknown.Available || unknown.Freshness.SyncStatus != "UNKNOWN" || !unknown.Freshness.Stale || unknown.Freshness.SyncedAt != at {
		t.Fatalf("unknown=%+v err=%v", unknown, err)
	}
	if err := db.Exec("DELETE FROM business_dashboard_snapshot").Error; err != nil {
		t.Fatal(err)
	}
	putBusinessFreshnessSnapshot(t, db, "finance", domain.BusinessSnapshot{Available: true, TenantID: "t", SnapshotAt: at, Metrics: map[string]any{}})
	legacy, err := repository.LatestBusiness(context.Background(), "t", "finance")
	if err != nil || legacy.Freshness.SyncStatus != "SNAPSHOT" || legacy.Freshness.SyncedAt != at {
		t.Fatalf("legacy=%+v err=%v", legacy, err)
	}
	if err := db.Exec("DELETE FROM business_dashboard_snapshot").Error; err != nil {
		t.Fatal(err)
	}
	putBusinessFreshnessSnapshot(t, db, "finance", domain.BusinessSnapshot{Available: true, TenantID: "other", SnapshotAt: at, SourceCode: "project_management", Metrics: map[string]any{}})
	if _, err := repository.LatestBusiness(context.Background(), "t", "finance"); err == nil {
		t.Fatal("invalid tenant/source accepted")
	}
	if err := db.Exec("DELETE FROM business_dashboard_snapshot").Error; err != nil {
		t.Fatal(err)
	}
	putBusinessFreshnessSnapshot(t, db, "finance", domain.BusinessSnapshot{Available: true, TenantID: "t", SnapshotAt: at, SourceCode: "project_management", Metrics: map[string]any{}})
	if _, err := repository.LatestBusiness(context.Background(), "t", "finance"); err == nil {
		t.Fatal("cross-source fallback accepted")
	}
}

// TestStandardBusinessFreshnessIgnoresIntegrationDisable 验证标准财务来源独立于外部总开关。
// 参数：t 为测试上下文。
// 返回值：无；独立发布被外部开关标旧或变零时失败。
func TestStandardBusinessFreshnessIgnoresIntegrationDisable(t *testing.T) {
	db := businessFreshnessDB(t)
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second).Format(time.RFC3339)
	payload, err := json.Marshal(domain.BusinessSnapshot{Available: true, TenantID: "t", SnapshotAt: at, Metrics: map[string]any{"currencies": []any{}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Exec("INSERT INTO standard_finance_source VALUES('t','standard-finance','b',1,?)", at).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Exec("INSERT INTO standard_finance_batch VALUES('t','standard-finance','b',1,?)", string(payload)).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("DROP TABLE sync_source").Error; err != nil {
		t.Fatal(err)
	}
	got, err := NewGORMRepositoryWithLocation(db, time.UTC).WithFinanceSource("standard").WithIntegrationSyncEnabled(false).LatestBusiness(context.Background(), "t", "finance")
	if err != nil || !got.Available || got.SourceCode != "standard-finance" || got.Freshness.Stale || got.Freshness.SyncStatus != "SNAPSHOT" || got.Freshness.SyncedAt != at {
		t.Fatalf("standard=%+v err=%v", got, err)
	}
}

// TestBusinessFreshnessDoesNotBorrowOtherSectionSuccess 验证报告旧快照不借投影成功变新。
// 参数：t 为隔离测试上下文。
// 返回值：无；共享来源最新成功覆盖本节水位时失败。
func TestBusinessFreshnessDoesNotBorrowOtherSectionSuccess(t *testing.T) {
	db := businessFreshnessDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-72 * time.Hour).Format(time.RFC3339)
	putBusinessFreshnessSnapshot(t, db, "report", domain.BusinessSnapshot{Available: true, TenantID: "t", SnapshotAt: old, SourceCode: "project_management", Metrics: map[string]any{"active_reports": 0}})
	if err := db.Exec("INSERT INTO sync_source VALUES('s','t','project_management',1,'SUCCESS',?,'',?)", now, now).Error; err != nil {
		t.Fatal(err)
	}
	got, err := NewGORMRepositoryWithLocation(db, time.UTC).LatestBusiness(context.Background(), "t", "report")
	if err != nil || !got.Available || !got.Freshness.Stale || got.SnapshotAt != old || got.Freshness.SyncedAt != old || !strings.Contains(got.Freshness.StaleReason, "48") {
		t.Fatalf("old report=%+v err=%v", got, err)
	}
}
