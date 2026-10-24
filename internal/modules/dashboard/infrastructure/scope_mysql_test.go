package infrastructure

import (
	"context"
	"fmt"
	"github.com/unified-identity-auth-platform/data-analysis/internal/aggregation"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestDashboardOrganizationAndTeamScopeOnMySQL(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("DASHBOARD_TEST_DSN"))
	if dsn == "" {
		t.Skip("DASHBOARD_TEST_DSN is required")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	// Requires the real migration 1-21 schema; never repairs or replaces it.
	tenant := fmt.Sprintf("b4-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		for _, table := range []string{"dim_contract", "dim_project", "sync_source", "project_projection_state"} {
			if err := db.Table(table).Where("tenant_id=?", tenant).Delete(map[string]interface{}{}).Error; err != nil {
				t.Errorf("cleanup %s: %v", table, err)
			}
		}
	})
	now := time.Now().UTC()
	if err = db.Exec(`DELETE FROM dim_contract WHERE tenant_id=?`, tenant).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Exec(`DELETE FROM dim_project WHERE tenant_id=?`, tenant).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Exec(`INSERT INTO dim_contract(tenant_id,contract_id,contract_number,title,contract_type,service_type,owner_user_id,status,amount_minor,end_date,start_date,owner_org_id,updated_at) VALUES
		(?, 'c-a', 'HT-A', 'A 合同', 'test', 'test', 'test-owner', 'ACTIVE', 10000, '2026-10-18', '2026-10-01', 'org-a', ?),
		(?, 'c-b', 'HT-B', 'B 合同', 'test', 'test', 'test-owner', 'ACTIVE', 20000, '2026-10-18', '2026-10-02', 'org-b', ?)`, tenant, now, tenant, now).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Exec(`INSERT INTO dim_project(tenant_id,project_id,name,customer,contract,status,health,owner_org_id,snapshot_id,risk,in_flight,service_items,source_updated_at,created_at,updated_at) VALUES
		(?, 'p-a', 'A', 'C', 'HT', 'COMPLETED', 'HEALTHY', 'org-a', 'snap', 1, 1, 7, ?, ?, ?), (?, 'p-b', 'B', 'C', 'HT', 'TERMINATED', 'HEALTHY', 'org-b', 'snap', 0, 0, 3, ?, ?, ?)`, tenant, now, now, now, tenant, now, now, now).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Exec(`DELETE FROM sync_source WHERE tenant_id=?`, tenant).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Exec(`INSERT INTO sync_source(id,tenant_id,subsystem_code,db_schema,db_host,read_account_ref,created_at,enabled,last_run_at,last_status,updated_at)
		VALUES (?, ?, 'contract_management', ?, 'test.invalid', 'test-only', ?, 1, ?, 'SUCCESS', ?)
		ON DUPLICATE KEY UPDATE last_run_at=VALUES(last_run_at), last_status=VALUES(last_status), enabled=VALUES(enabled), updated_at=VALUES(updated_at)`, tenant, tenant, tenant, now, now, now).Error; err != nil {
		t.Fatal(err)
	}
	publishTestProjection(t, db, tenant, 2)
	repository := NewGORMRepository(db)
	orgCtx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: tenant, UserID: "sales-director", ScopeMode: "ORG", OrganizationIDs: []string{"org-a"}})
	contract, available, err := repository.LatestContract(orgCtx, tenant)
	if err != nil || !available || contract.TotalAmountMinor != 10000 || contract.TotalContracts != 1 {
		t.Fatalf("org contract=%+v available=%v err=%v", contract, available, err)
	}
	if contract.Freshness.SyncStatus != domain.SyncStatusSuccess || contract.Freshness.Stale {
		t.Fatalf("org contract freshness=%+v, want successful sync watermark", contract.Freshness)
	}
	if contract.Freshness.SourceUpdatedAt == "" || contract.SnapshotAt != contract.Freshness.SourceUpdatedAt {
		t.Fatalf("org contract snapshot_at=%q freshness=%+v, want source update time", contract.SnapshotAt, contract.Freshness)
	}
	details, total, err := repository.ListContracts(orgCtx, tenant, 1, 20)
	if err != nil || total != 1 || len(details) != 1 || details[0].ContractNumber != "HT-A" {
		t.Fatalf("org details=%+v total=%d err=%v", details, total, err)
	}
	project, available, err := repository.LatestProject(orgCtx, tenant)
	if err != nil || !available || project.ProjectCount != 1 || project.StatusCounts["COMPLETED"] != 1 || project.RiskProjects != 1 || project.InFlightProjects != 1 || project.ServiceItems != 7 {
		t.Fatalf("org project=%+v available=%v err=%v", project, available, err)
	}
	// Published projection has an independent successful watermark.
	if project.Freshness.SyncStatus != domain.SyncStatusSuccess || project.Freshness.Stale {
		t.Fatalf("org project freshness=%+v, want successful projection status", project.Freshness)
	}
	trend, err := repository.ListTrend(orgCtx, tenant, 12)
	if err != nil || len(trend) != 1 || trend[0].ContractAmountMinor != 10000 || trend[0].ProjectCount != 1 {
		t.Fatalf("org trend=%+v err=%v", trend, err)
	}
	teamCtx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: tenant, UserID: "tech-director", ScopeMode: "TEAM", TeamIDs: []string{"org-b"}})
	teamContract, available, err := repository.LatestContract(teamCtx, tenant)
	if err != nil || !available || teamContract.TotalAmountMinor != 20000 || teamContract.TotalContracts != 1 {
		t.Fatalf("team contract=%+v available=%v err=%v", teamContract, available, err)
	}
	teamProject, available, err := repository.LatestProject(teamCtx, tenant)
	if err != nil || !available || teamProject.ProjectCount != 1 || teamProject.InFlightProjects != 0 || teamProject.RiskProjects != 0 || teamProject.ServiceItems != 3 {
		t.Fatalf("team project=%+v available=%v err=%v", teamProject, available, err)
	}
	// Source creation month must survive a much newer publish/update time.
	if err := db.Exec("UPDATE dim_project SET created_at='2026-02-03' WHERE tenant_id=?", tenant).Error; err != nil {
		t.Fatal(err)
	}
	counts, err := repository.projectCreationTrend(orgCtx, scopeFor(orgCtx, tenant))
	if err != nil || len(counts) != 1 || counts["2026-02"] != 1 {
		t.Fatalf("source creation trend=%v err=%v", counts, err)
	}
	if err := db.Model(&aggregation.ProjectProjectionState{}).Where("tenant_id=?", tenant).Update("status", "FAILED").Error; err != nil {
		t.Fatal(err)
	}
	retained, available, err := repository.LatestProject(orgCtx, tenant)
	if err != nil || !available || !retained.Freshness.Stale || retained.Freshness.SyncedAt != project.Freshness.SyncedAt {
		t.Fatalf("retained=%+v available=%v err=%v", retained, available, err)
	}
	if err := db.Exec("UPDATE dim_project SET owner_org_id='org-b' WHERE tenant_id=?", tenant).Error; err != nil {
		t.Fatal(err)
	}
	moved, available, err := repository.LatestProject(orgCtx, tenant)
	if err != nil || !available || moved.ProjectCount != 0 {
		t.Fatalf("old scope=%+v available=%v err=%v", moved, available, err)
	}
	noneCtx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: tenant, UserID: "no-scope", ScopeMode: "NONE"})
	noneProject, available, err := repository.LatestProject(noneCtx, tenant)
	if err != nil || available || noneProject.ProjectCount != 0 || noneProject.Freshness.SyncedAt != "" {
		t.Fatalf("NONE project=%+v available=%v err=%v", noneProject, available, err)
	}
	noneContract, available, err := repository.LatestContract(noneCtx, tenant)
	if err != nil || available || noneContract.TotalContracts != 0 {
		t.Fatalf("none contract=%+v available=%v err=%v", noneContract, available, err)
	}
}
