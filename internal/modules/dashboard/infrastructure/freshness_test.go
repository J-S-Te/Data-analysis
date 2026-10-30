package infrastructure

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/aggregation"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// freshnessTestDB 建立隔离的内存统计夹具并补齐来源列。
// 参数：t为测试上下文。
// 返回值：供本用例使用的GORM连接，初始化失败终止用例。
func freshnessTestDB(t *testing.T) *gorm.DB {
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
	for _, ddl := range []string{
		`CREATE TABLE dim_contract (origin_kind TEXT DEFAULT 'NORMAL',
			tenant_id TEXT, contract_id TEXT, contract_number TEXT, title TEXT, status TEXT,
			amount_minor INTEGER, currency TEXT, end_date DATE, start_date DATE,
			owner_org_id TEXT, updated_at DATETIME)`,
		`CREATE TABLE dim_project (
			tenant_id TEXT, project_id TEXT, status TEXT, health TEXT,
			owner_org_id TEXT, snapshot_id TEXT, risk BOOLEAN, in_flight BOOLEAN, service_items INTEGER, source_updated_at DATETIME, created_at DATETIME, updated_at DATETIME)`,
		`CREATE TABLE sync_source (
			id TEXT, tenant_id TEXT, subsystem_code TEXT, db_schema TEXT, enabled BOOLEAN,
			last_watermark DATETIME, last_run_at DATETIME, last_status TEXT, last_error TEXT, updated_at DATETIME)`,
		`CREATE TABLE api_contract_dashboard (
			tenant_id TEXT, snapshot_at TEXT, total_amount_minor INTEGER, currency_totals_json TEXT,
			total_contracts INTEGER, approval_contracts INTEGER, active_contracts INTEGER, expired_contracts INTEGER)`,
		`CREATE TABLE api_project_dashboard (
			tenant_id TEXT, snapshot_at TEXT, project_count INTEGER, in_flight_projects INTEGER,
			risk_projects INTEGER, service_items INTEGER, status_counts_json TEXT)`,
		`CREATE TABLE fct_opportunity_conversion (
			tenant_id TEXT, opportunity_count INTEGER, won_contract_count INTEGER)`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AutoMigrate(&aggregation.ProjectProjectionState{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func orgScopeContext(tenant, org string) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: tenant, UserID: "sales-director", ScopeMode: "ORG", OrganizationIDs: []string{org},
	})
}

// A12：组织/团队范围读 dim_contract 时，source_updated_at 来自源业务行，
// synced_at 来自同步水位，computed_at 才是本次查询时间；三者不能混用。
func TestScopedContractFreshnessSeparatesSourceSyncAndComputedTimes(t *testing.T) {
	db := freshnessTestDB(t)
	repository := NewGORMRepository(db)
	tenant := "tenant-freshness"
	sourceAt := time.Now().UTC().Add(-30 * time.Hour).Truncate(time.Second)
	syncedAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	if err := db.Exec(`INSERT INTO dim_contract
		(tenant_id, contract_id, contract_number, title, status, amount_minor, currency, end_date, start_date, owner_org_id, updated_at)
		VALUES (?, 'c-1', 'HT-1', '合同', 'ACTIVE', 100, 'CNY', NULL, NULL, 'org-a', ?)`, tenant, sourceAt).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO sync_source
		(id, tenant_id, subsystem_code, db_schema, enabled, last_run_at, last_status, updated_at)
		VALUES ('s-1', ?, 'contract_management', 'contract_management', 1, ?, 'SUCCESS', ?)`, tenant, syncedAt, syncedAt).Error; err != nil {
		t.Fatal(err)
	}

	// computed_at 按 RFC3339 秒级输出，比较窗口留出秒级截断余量。
	before := time.Now().UTC().Add(-2 * time.Second)
	snapshot, available, err := repository.LatestContract(orgScopeContext(tenant, "org-a"), tenant)
	after := time.Now().UTC().Add(2 * time.Second)
	if err != nil || !available || snapshot.TotalContracts != 1 {
		t.Fatalf("contract=%+v available=%v err=%v", snapshot, available, err)
	}
	gotSource := application.ParseDashboardTimestamp(snapshot.Freshness.SourceUpdatedAt)
	gotSynced := application.ParseDashboardTimestamp(snapshot.Freshness.SyncedAt)
	computed := application.ParseDashboardTimestamp(snapshot.Freshness.ComputedAt)
	if gotSource == nil || !gotSource.Equal(sourceAt) {
		t.Fatalf("source_updated_at=%q, want %s", snapshot.Freshness.SourceUpdatedAt, sourceAt)
	}
	if gotSynced == nil || !gotSynced.Equal(syncedAt) {
		t.Fatalf("synced_at=%q, want %s", snapshot.Freshness.SyncedAt, syncedAt)
	}
	if computed == nil || computed.Before(before) || computed.After(after) {
		t.Fatalf("computed_at=%q, want query time in [%s, %s]", snapshot.Freshness.ComputedAt, before, after)
	}
	if snapshot.Freshness.SyncStatus != domain.SyncStatusSuccess || snapshot.Freshness.Stale {
		t.Fatalf("freshness=%+v, want fresh success", snapshot.Freshness)
	}
	// 修复前这里被写成 time.Now()，把查询时间冒充成数据更新时间。
	if snapshot.SnapshotAt != snapshot.Freshness.SourceUpdatedAt {
		t.Fatalf("snapshot_at=%q, want source update time %q", snapshot.SnapshotAt, snapshot.Freshness.SourceUpdatedAt)
	}
}

// 源同步失败时保留旧数据，但必须标记陈旧并说明原因，且 synced_at 不能拿失败尝试顶替。
func TestScopedContractFreshnessMarksFailedSourceSyncStale(t *testing.T) {
	db := freshnessTestDB(t)
	repository := NewGORMRepository(db)
	tenant := "tenant-stale"
	sourceAt := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)
	if err := db.Exec(`INSERT INTO dim_contract
		(tenant_id, contract_id, contract_number, title, status, amount_minor, currency, owner_org_id, updated_at)
		VALUES (?, 'c-2', 'HT-2', '合同', 'ACTIVE', 200, 'CNY', 'org-a', ?)`, tenant, sourceAt).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO sync_source
		(id, tenant_id, subsystem_code, db_schema, enabled, last_run_at, last_status, updated_at)
		VALUES ('s-2', ?, 'contract_management', 'contract_management', 1, ?, 'FAILED', ?)`, tenant, time.Now().UTC().Add(-time.Hour), time.Now().UTC()).Error; err != nil {
		t.Fatal(err)
	}
	snapshot, available, err := repository.LatestContract(orgScopeContext(tenant, "org-a"), tenant)
	if err != nil || !available {
		t.Fatalf("available=%v err=%v", available, err)
	}
	if !snapshot.Freshness.Stale || !strings.Contains(snapshot.Freshness.StaleReason, "失败") {
		t.Fatalf("freshness=%+v, want stale failed-sync reason", snapshot.Freshness)
	}
	if snapshot.Freshness.SyncedAt != "" {
		t.Fatalf("synced_at=%q, want empty for failed sync", snapshot.Freshness.SyncedAt)
	}
	if snapshot.SnapshotAt != snapshot.Freshness.SourceUpdatedAt {
		t.Fatalf("snapshot_at=%q, want source update time", snapshot.SnapshotAt)
	}
}

// 旧维表没有完整发布标记时，项目指标及时间水位不可用。
func TestScopedProjectFreshnessReportsUnknownSyncWithoutFakingTime(t *testing.T) {
	db := freshnessTestDB(t)
	repository := NewGORMRepository(db)
	tenant := "tenant-project"
	updatedAt := time.Now().UTC().Add(-10 * time.Hour).Truncate(time.Second)
	if err := db.Exec(`INSERT INTO dim_project
		(tenant_id, project_id, status, health, owner_org_id, created_at, updated_at)
		VALUES (?, 'p-1', 'IN_PROGRESS', 'HEALTHY', 'org-a', ?, ?)`, tenant, updatedAt, updatedAt).Error; err != nil {
		t.Fatal(err)
	}
	snapshot, available, err := repository.LatestProject(orgScopeContext(tenant, "org-a"), tenant)
	if err != nil || available || snapshot.ProjectCount != 0 {
		t.Fatalf("project=%+v available=%v err=%v", snapshot, available, err)
	}
	if snapshot.Freshness.SyncStatus != domain.SyncStatusUnknown || !snapshot.Freshness.Stale {
		t.Fatalf("freshness=%+v, want unknown/stale sync status", snapshot.Freshness)
	}
	gotSource := application.ParseDashboardTimestamp(snapshot.Freshness.SourceUpdatedAt)
	if gotSource != nil {
		t.Fatalf("unpublished source time leaked: %q", snapshot.Freshness.SourceUpdatedAt)
	}
	if snapshot.SnapshotAt != snapshot.Freshness.SourceUpdatedAt {
		t.Fatalf("snapshot_at=%q, want source update time", snapshot.SnapshotAt)
	}
}

// 全租户持久快照分支不回归：snapshot_at 保持原值，computed_at 单列查询时间。
func TestTenantSnapshotFreshnessKeepsPersistedSnapshotSemantics(t *testing.T) {
	db := freshnessTestDB(t)
	repository := NewGORMRepository(db)
	tenant := "tenant-snapshot"
	storedAt := time.Now().UTC().Add(-24 * time.Hour).Format("2006-01-02 15:04:05.000")
	if err := db.Exec(`INSERT INTO api_contract_dashboard
		(tenant_id, snapshot_at, total_amount_minor, currency_totals_json, total_contracts, approval_contracts, active_contracts, expired_contracts)
		VALUES (?, ?, 300, '[]', 2, 0, 2, 0)`, tenant, storedAt).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO api_project_dashboard
		(tenant_id, snapshot_at, project_count, in_flight_projects, risk_projects, service_items, status_counts_json)
		VALUES (?, ?, 4, 3, 1, 4, '{"IN_PROGRESS":3,"RISK":1}')`, tenant, storedAt).Error; err != nil {
		t.Fatal(err)
	}
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: tenant, UserID: "boss", ScopeMode: "TENANT"})

	contract, available, err := repository.LatestContract(ctx, tenant)
	if err != nil || !available {
		t.Fatalf("contract available=%v err=%v", available, err)
	}
	if contract.SnapshotAt != storedAt || contract.Freshness.SyncedAt != storedAt {
		t.Fatalf("snapshot_at=%q synced_at=%q, want stored snapshot time %q", contract.SnapshotAt, contract.Freshness.SyncedAt, storedAt)
	}
	if contract.Freshness.SyncStatus != domain.SyncStatusSnapshot || contract.Freshness.Stale {
		t.Fatalf("contract freshness=%+v, want persisted snapshot status", contract.Freshness)
	}
	computed := application.ParseDashboardTimestamp(contract.Freshness.ComputedAt)
	if computed == nil || computed.Before(time.Now().UTC().Add(-time.Minute)) {
		t.Fatalf("computed_at=%q, want current query time", contract.Freshness.ComputedAt)
	}

	project, available, err := repository.LatestProject(ctx, tenant)
	if err != nil || !available || project.ProjectCount != 4 {
		t.Fatalf("project=%+v available=%v err=%v", project, available, err)
	}
	if project.SnapshotAt != storedAt || project.Freshness.SyncStatus != domain.SyncStatusSnapshot || project.Freshness.Stale {
		t.Fatalf("project snapshot=%+v freshness=%+v, want unchanged persisted snapshot", project, project.Freshness)
	}
	if project.Freshness.ComputedAt == "" {
		t.Fatal("computed_at must be present for persisted snapshots")
	}
}

func TestTenantSnapshotFreshnessMarksExpiredDataStale(t *testing.T) {
	db := freshnessTestDB(t)
	repository := NewGORMRepository(db)
	tenant := "tenant-expired-snapshot"
	storedAt := time.Now().UTC().Add(-49 * time.Hour).Format("2006-01-02 15:04:05.000")
	if err := db.Exec(`INSERT INTO api_contract_dashboard
		(tenant_id, snapshot_at, total_amount_minor, currency_totals_json, total_contracts, approval_contracts, active_contracts, expired_contracts)
		VALUES (?, ?, 300, '[]', 2, 0, 2, 0)`, tenant, storedAt).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO api_project_dashboard
		(tenant_id, snapshot_at, project_count, in_flight_projects, risk_projects, service_items, status_counts_json)
		VALUES (?, ?, 4, 3, 1, 4, '{"IN_PROGRESS":3,"RISK":1}')`, tenant, storedAt).Error; err != nil {
		t.Fatal(err)
	}
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: tenant, UserID: "boss", ScopeMode: "TENANT"})
	contract, available, err := repository.LatestContract(ctx, tenant)
	if err != nil || !available || contract.TotalContracts != 2 || contract.TotalAmountMinor != 300 {
		t.Fatalf("contract=%+v available=%v err=%v, want retained persisted data", contract, available, err)
	}
	project, available, err := repository.LatestProject(ctx, tenant)
	if err != nil || !available || project.ProjectCount != 4 || project.ServiceItems != 4 || project.StatusCounts["RISK"] != 1 {
		t.Fatalf("project=%+v available=%v err=%v, want retained persisted data", project, available, err)
	}
	for _, snapshot := range []struct {
		name       string
		snapshotAt string
		freshness  domain.DataFreshness
	}{
		{name: "contract", snapshotAt: contract.SnapshotAt, freshness: contract.Freshness},
		{name: "project", snapshotAt: project.SnapshotAt, freshness: project.Freshness},
	} {
		if !snapshot.freshness.Stale || !strings.Contains(snapshot.freshness.StaleReason, "48") {
			t.Errorf("%s freshness=%+v, want expired snapshot marker", snapshot.name, snapshot.freshness)
		}
		if snapshot.snapshotAt != storedAt || snapshot.freshness.SyncedAt != storedAt || snapshot.freshness.SyncStatus != domain.SyncStatusSnapshot {
			t.Errorf("%s snapshot_at=%q freshness=%+v, want unchanged persisted snapshot facts", snapshot.name, snapshot.snapshotAt, snapshot.freshness)
		}
		if snapshot.freshness.SourceUpdatedAt != "" || snapshot.freshness.ComputedAt == "" {
			t.Errorf("%s freshness=%+v, want no invented source time and a separate query time", snapshot.name, snapshot.freshness)
		}
	}
}

func TestTenantSnapshotFreshnessDoesNotInventMissingSnapshots(t *testing.T) {
	repository := NewGORMRepository(freshnessTestDB(t))
	tenant := "tenant-no-snapshot"
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: tenant, UserID: "boss", ScopeMode: "TENANT"})
	contract, available, err := repository.LatestContract(ctx, tenant)
	if err != nil || available || contract.SnapshotAt != "" || contract.Freshness.SyncedAt != "" || contract.Freshness.SourceUpdatedAt != "" {
		t.Fatalf("contract=%+v available=%v err=%v, want unavailable without invented data times", contract, available, err)
	}
	project, available, err := repository.LatestProject(ctx, tenant)
	if err != nil || available || project.SnapshotAt != "" || project.Freshness.SyncedAt != "" || project.Freshness.SourceUpdatedAt != "" {
		t.Fatalf("project=%+v available=%v err=%v, want unavailable without invented data times", project, available, err)
	}
}

func publishTestProjection(t *testing.T, db *gorm.DB, tenant string, total int) {
	t.Helper()
	now := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	state := aggregation.ProjectProjectionState{TenantID: tenant, ProjectionCode: aggregation.ProjectProjectionCode, Status: "SUCCESS", LastSuccessAt: &now, PublishedRunID: "published-run", PublishedSnapshotID: "snap", PublishedTotal: total, PublishedSourceSnapshotAt: &now, PublishedChecksumSHA256: strings.Repeat("a", 64)}
	if err := db.Create(&state).Error; err != nil {
		t.Fatal(err)
	}
}
func TestScopedProjectUsesPublishedSourceMetrics(t *testing.T) {
	db := freshnessTestDB(t)
	tenant := "metrics"
	repo := NewGORMRepository(db)
	publishTestProjection(t, db, tenant, 3)
	now := time.Now().UTC().Add(-2 * time.Hour)
	for _, row := range []struct {
		id, status, org string
		risk, flight    bool
		items           int
	}{{"a", "已完成", "org-a", true, false, 7}, {"b", "全部终止", "org-a", false, true, 3}, {"c", "RISK", "org-b", false, false, 11}} {
		if err := db.Exec(`INSERT INTO dim_project (tenant_id,project_id,status,owner_org_id,snapshot_id,risk,in_flight,service_items,source_updated_at,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`, tenant, row.id, row.status, row.org, "snap", row.risk, row.flight, row.items, now, now, now.Add(time.Hour)).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, mode := range []string{"ORG", "TEAM", "NONE"} {
		ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: tenant, ScopeMode: mode, OrganizationIDs: []string{"org-a"}, TeamIDs: []string{"org-a"}})
		got, ok, err := repo.LatestProject(ctx, tenant)
		if err != nil {
			t.Fatal(err)
		}
		if mode == "NONE" {
			if ok || got.ProjectCount != 0 || got.Freshness.SyncedAt != "" {
				t.Fatalf("NONE leaked %+v", got)
			}
			continue
		}
		want, items := 2, 10
		if mode == "TENANT" {
			want = 3
			items = 21
		}
		if got.Freshness.SourceUpdatedAt != application.FormatDashboardTimestamp(&now) {
			t.Fatalf("source timestamp replaced: %+v", got.Freshness)
		}
		if !ok || got.ProjectCount != want || got.InFlightProjects != 1 || got.RiskProjects != 1 || got.ServiceItems != items || got.Freshness.Stale {
			t.Fatalf("%s got=%+v available=%v", mode, got, ok)
		}
	}
	for _, mode := range []string{"ORG", "TEAM"} {
		ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: tenant, ScopeMode: mode})
		empty, ok, err := repo.LatestProject(ctx, tenant)
		if err != nil || ok || empty.ProjectCount != 0 || empty.Freshness.SyncedAt != "" {
			t.Fatalf("missing IDs leaked: %+v available=%v err=%v", empty, ok, err)
		}
	}
	if err := db.Exec(`UPDATE dim_project SET owner_org_id='org-b' WHERE tenant_id=?`, tenant).Error; err != nil {
		t.Fatal(err)
	}
	got, ok, err := repo.LatestProject(orgScopeContext(tenant, "org-a"), tenant)
	if err != nil || !ok || got.ProjectCount != 0 {
		t.Fatalf("moved org got=%+v available=%v err=%v", got, ok, err)
	}
}
func TestScopedProjectVerifiedEmptyVersusUnknown(t *testing.T) {
	db := freshnessTestDB(t)
	repo := NewGORMRepository(db)
	tenant := "empty"
	got, ok, err := repo.LatestProject(orgScopeContext(tenant, "org-a"), tenant)
	if err != nil || ok || got.Freshness.SyncStatus != domain.SyncStatusUnknown {
		t.Fatalf("unknown=%+v available=%v err=%v", got, ok, err)
	}
	publishTestProjection(t, db, tenant, 0)
	got, ok, err = repo.LatestProject(orgScopeContext(tenant, "org-a"), tenant)
	if err != nil || !ok || got.ProjectCount != 0 || got.ServiceItems != 0 || got.Freshness.Stale || got.Freshness.SourceUpdatedAt != "" {
		t.Fatalf("verified empty=%+v available=%v err=%v", got, ok, err)
	}
	for _, status := range []string{"RUNNING", "FAILED"} {
		if err := db.Model(&aggregation.ProjectProjectionState{}).Where("tenant_id=?", tenant).Update("status", status).Error; err != nil {
			t.Fatal(err)
		}
		failed, ok, err := repo.LatestProject(orgScopeContext(tenant, "org-a"), tenant)
		if err != nil || !ok || !failed.Freshness.Stale || failed.Freshness.SyncedAt != got.Freshness.SyncedAt {
			t.Fatalf("retained=%+v available=%v err=%v", failed, ok, err)
		}
	}
	if err := db.Model(&aggregation.ProjectProjectionState{}).Where("tenant_id=?", tenant).Update("status", "SUCCESS").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO sync_source (id,tenant_id,subsystem_code,db_schema,enabled,last_status) VALUES ('disabled',?,'project_management','test',0,'SUCCESS')`, tenant).Error; err != nil {
		t.Fatal(err)
	}
	disabled, ok, err := repo.LatestProject(orgScopeContext(tenant, "org-a"), tenant)
	if err != nil || !ok || !disabled.Freshness.Stale || !strings.Contains(disabled.Freshness.StaleReason, "停用") || disabled.Freshness.SyncedAt != got.Freshness.SyncedAt {
		t.Fatalf("disabled=%+v available=%v err=%v", disabled, ok, err)
	}

}

func TestScopedProjectCreationTrendUsesSourceMonth(t *testing.T) {
	db := freshnessTestDB(t)
	repo := NewGORMRepository(db)
	tenant := "trend-source"
	publishTestProjection(t, db, tenant, 2)
	created := time.Date(2026, 2, 3, 0, 0, 0, 0, time.UTC)
	updated := created.AddDate(0, 6, 0)
	if err := db.Exec(`INSERT INTO dim_project (tenant_id,project_id,status,owner_org_id,snapshot_id,risk,in_flight,service_items,source_updated_at,created_at,updated_at) VALUES (?,'p','完成','org-a','snap',0,0,4,?,?,?)`, tenant, updated, created, updated).Error; err != nil {
		t.Fatal(err)
	}
	got, err := repo.projectCreationTrend(context.Background(), queryScope{mode: "ORG", tenantID: tenant, orgUnitIDs: []string{"org-a"}})
	if err != nil || len(got) != 1 || got["2026-02"] != 1 {
		t.Fatalf("trend=%v err=%v", got, err)
	}
	if err := db.Exec(`DELETE FROM project_projection_state WHERE tenant_id=?`, tenant).Error; err != nil {
		t.Fatal(err)
	}
	got, err = repo.projectCreationTrend(context.Background(), queryScope{mode: "ORG", tenantID: tenant, orgUnitIDs: []string{"org-a"}})
	if err != nil || len(got) != 0 {
		t.Fatalf("unpublished trend leaked %v err=%v", got, err)
	}
}
