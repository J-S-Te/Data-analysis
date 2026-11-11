package infrastructure

import (
	"context"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"testing"
	"time"
)

// TestScopedContractRequiresSuccessAndKeepsRealZero 验证成功凭据与真实零值边界。
// 参数：t 为隔离单元测试上下文。
// 返回值：无；不符合可用性语义时失败。
func TestScopedContractRequiresSuccessAndKeepsRealZero(t *testing.T) {
	db := scopedContractFixture(t)

	repo := NewGORMRepositoryWithLocation(db, time.UTC)
	scope := queryScope{mode: "ORG", tenantID: "t", orgUnitIDs: []string{"o"}}
	insertScopedContract(t, db, "NORMAL", "t", "c", "active", "", "u", "o")
	if err := db.Exec("DELETE FROM sync_source").Error; err != nil {
		t.Fatal(err)
	}
	_, available, err := repo.scopedContract(context.Background(), scope)
	if err != nil || available {
		t.Fatalf("unpublished available=%v err=%v", available, err)
	}
	if err := db.Exec("DELETE FROM dim_contract").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO sync_source(id,tenant_id,subsystem_code,enabled,last_status,last_success_at,updated_at) VALUES('s','t','contract_management',1,'FAILED',?,?)", time.Now(), time.Now()).Error; err != nil {
		t.Fatal(err)
	}
	got, available, err := repo.scopedContract(context.Background(), scope)
	if err != nil || !available || got.TotalContracts != 0 || !got.Freshness.Stale {
		t.Fatalf("published zero=%+v available=%v err=%v", got, available, err)
	}
}

// TestScopedConversionUsesBothOwnerScopes 验证分母归属商机、分子归属合同。
// 参数：t 为测试上下文。
// 返回值：无；跨范围或作废记录进入转化时失败。
func TestScopedConversionUsesBothOwnerScopes(t *testing.T) {
	db := scopedContractFixture(t)
	for _, q := range []string{

		"CREATE TABLE dim_opportunity(tenant_id TEXT,opportunity_id INTEGER,contract_id TEXT,current_stage TEXT,opp_status TEXT,deleted_at DATETIME,owner_user_id TEXT,owner_org_id TEXT)",
		"INSERT INTO sync_source(id,tenant_id,subsystem_code,enabled,last_status,last_success_at,updated_at) VALUES('s','t','contract_management',1,'SUCCESS',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP),('r','t','customer_and_opportunity',1,'SUCCESS',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)",
		"INSERT INTO dim_opportunity VALUES('t',1,'c1','已签约','CLOSED',NULL,'u','o'),('t',2,'c2','已签约','CLOSED',NULL,'u','o'),('t',3,'c3','已签约','CLOSED',NULL,'x','x'),('t',4,NULL,'跟进','VOID',NULL,'u','o')",
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	insertScopedContract(t, db, "NORMAL", "t", "c1", "active", "", "u", "o")
	insertScopedContract(t, db, "NORMAL", "t", "c2", "active", "", "x", "x")
	insertScopedContract(t, db, "NORMAL", "t", "c3", "active", "", "u", "o")
	got, available, err := NewGORMRepositoryWithLocation(db, time.UTC).scopedContract(context.Background(), queryScope{mode: "ORG", tenantID: "t", orgUnitIDs: []string{"o"}})
	if err != nil || !available || !got.ConversionAvailable || got.OpportunityCount != 2 || got.WonContractCount != 1 {
		t.Fatalf("conversion=%+v available=%v err=%v", got, available, err)
	}
}

// TestTrendDoesNotSumCurrencies 验证趋势只保留原币种金额。
// 参数：t 为测试上下文。
// 返回值：无；混币总额非零时失败。
func TestTrendDoesNotSumCurrencies(t *testing.T) {
	facts := []struct {
		Period, Currency string
		Amount, Count    int64
	}{{"2026-11", "CNY", 100, 1}, {"2026-11", "USD", 200, 2}}
	got := trendRowsByCurrency(facts, 12)
	if len(got) != 1 || got[0].ContractAmountMinor != 0 || got[0].ContractCount != 3 || len(got[0].CurrencyTotals) != 2 {
		t.Fatalf("trend=%+v", got)
	}
}

// TestProjectDetailsPublishedScopeAndPagination 验证项目明细只读取已发布代次及可信范围。
// 参数：t 为隔离测试上下文。
// 返回值：无；可用性、范围和分页不一致时失败。
func TestProjectDetailsPublishedScopeAndPagination(t *testing.T) {
	db := freshnessTestDB(t)
	for _, q := range []string{"ALTER TABLE dim_project ADD COLUMN name TEXT", "ALTER TABLE dim_project ADD COLUMN manager TEXT"} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	repo := NewGORMRepositoryWithLocation(db, time.UTC)
	ctx := orgScopeContext("t", "o")
	empty, err := repo.ListProjects(ctx, "t", 1, 1)
	if err != nil || empty.Available {
		t.Fatalf("unpublished=%+v err=%v", empty, err)
	}
	publishTestProjection(t, db, "t", 3)
	for _, row := range []struct{ id, org, snap string }{{"p1", "o", "snap"}, {"p2", "o", "snap"}, {"p3", "x", "snap"}, {"p4", "o", "old"}} {
		if err := db.Exec("INSERT INTO dim_project(tenant_id,project_id,name,manager,status,owner_org_id,snapshot_id,report_stage,service_items,risk,in_flight) VALUES('t',?,'项目','经理','实施中',?,?,'RECTIFYING',2,0,1)", row.id, row.org, row.snap).Error; err != nil {
			t.Fatal(err)
		}
	}
	got, err := repo.ListProjects(ctx, "t", 2, 1)
	if err != nil || !got.Available || got.Total != 2 || len(got.Items) != 1 || got.Items[0].ProjectID != "p1" {
		t.Fatalf("details=%+v err=%v", got, err)
	}
}

// TestTrendSourcePermissions 验证总览权限不隐式授予合同与项目数据读取。
// 参数：t 为隔离测试上下文。
// 返回值：无；未授权来源被查询或泄漏时失败。
func TestTrendSourcePermissions(t *testing.T) {
	db := scopedContractFixture(t)
	repo := NewGORMRepositoryWithLocation(db, time.UTC)
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "t", ScopeMode: "TENANT", Permissions: map[string]struct{}{"dashboard.overview.view": {}}})
	points, err := repo.ListTrend(ctx, "t", 12)
	if err != nil || len(points) != 0 {
		t.Fatalf("overview-only points=%+v err=%v", points, err)
	}
	for _, query := range []string{
		"CREATE TABLE fct_contract_signing(tenant_id TEXT,period_month TEXT,currency TEXT,sign_amount_minor INTEGER,contract_count INTEGER)",
		"INSERT INTO fct_contract_signing VALUES('t','2026-11','USD',100,2)",
	} {
		if err := db.Exec(query).Error; err != nil {
			t.Fatal(err)
		}
	}
	ctx = auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "t", ScopeMode: "TENANT", Permissions: map[string]struct{}{"dashboard.contract.view": {}}})
	points, err = repo.ListTrend(ctx, "t", 12)
	if err != nil || len(points) != 1 || points[0].ContractCount != 2 || !points[0].ContractAvailable || points[0].ProjectAvailable {
		t.Fatalf("contract-only points=%+v err=%v", points, err)
	}
}

// TestIntegrationDisabledKeepsSnapshotStale 验证停用外部同步只标旧不隐藏历史成果。
// 参数：t 为隔离单元测试上下文。
// 返回值：无；水位丢失或新鲜度伪装时失败。
func TestIntegrationDisabledKeepsSnapshotStale(t *testing.T) {
	now := time.Now().UTC()
	repository := NewGORMRepositoryWithLocation(nil, time.UTC).WithIntegrationSyncEnabled(false)
	result := repository.buildFreshness(application.FreshnessInput{PersistedSnapshot: true, PersistedSnapshotAt: now.Format(time.RFC3339)}, now)
	if !result.Stale || result.SyncedAt == "" {
		t.Fatalf("freshness=%+v", result)
	}
}
