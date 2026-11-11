package infrastructure

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
	"gorm.io/gorm"
)

// conversionFreshnessFixture 建立成功合同与带明确CRM水位的隔离夹具。
// 参数：t 为测试上下文，status/enabled 为CRM当前状态，successAt 为上次成功时间。
// 返回值：仓储与数据库；夹具只包含同范围和范围外的合成事实。
func conversionFreshnessFixture(t *testing.T, status string, enabled bool, successAt time.Time) (*GORMRepository, *gorm.DB) {
	t.Helper()
	db := scopedContractFixture(t)
	for _, query := range []string{
		"CREATE TABLE dim_opportunity(tenant_id TEXT,opportunity_id INTEGER,contract_id TEXT,current_stage TEXT,opp_status TEXT,deleted_at DATETIME,owner_user_id TEXT,owner_org_id TEXT)",
		"INSERT INTO dim_opportunity VALUES('t',1,'c1','已签约','CLOSED',NULL,'u','o'),('t',2,'c2','已签约','CLOSED',NULL,'x','x')",
	} {
		if err := db.Exec(query).Error; err != nil {
			t.Fatal(err)
		}
	}
	insertScopedContract(t, db, "NORMAL", "t", "c1", "active", "", "u", "o")
	insertScopedContract(t, db, "NORMAL", "t", "c2", "active", "", "x", "x")
	if err := db.Exec("INSERT INTO sync_source(id,tenant_id,subsystem_code,enabled,last_status,last_success_at,updated_at) VALUES('crm','t','customer_and_opportunity',?,?,?,?)", enabled, status, successAt, time.Now().UTC()).Error; err != nil {
		t.Fatal(err)
	}
	return NewGORMRepositoryWithLocation(db, time.UTC), db
}

// TestScopedConversionCRMDegradationMarksOverall 验证CRM失败、停用和过期不能伪装整体新鲜。
// 参数：t 为隔离回归上下文。
// 返回值：无；旧转化事实被隐藏、越权或缺CRM陈旧说明时失败。
func TestScopedConversionCRMDegradationMarksOverall(t *testing.T) {
	cases := []struct {
		name, status string
		enabled      bool
		age          time.Duration
		stale        bool
	}{
		{"failed", "FAILED", true, time.Hour, true},
		{"running", "RUNNING", true, time.Hour, true},
		{"disabled", "SUCCESS", false, time.Hour, true},
		{"aged", "SUCCESS", true, 49 * time.Hour, true},
		{"fresh", "SUCCESS", true, time.Hour, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, _ := conversionFreshnessFixture(t, tc.status, tc.enabled, time.Now().UTC().Add(-tc.age))
			got, available, err := repo.scopedContract(context.Background(), queryScope{mode: "ORG", tenantID: "t", orgUnitIDs: []string{"o"}})
			if err != nil || !available || !got.ConversionAvailable || got.OpportunityCount != 1 || got.WonContractCount != 1 {
				t.Fatalf("old scoped facts changed: %+v available=%v err=%v", got, available, err)
			}
			if got.Freshness.Stale != tc.stale {
				t.Fatalf("CRM %s freshness=%+v", tc.name, got.Freshness)
			}
			if tc.stale && !strings.Contains(got.Freshness.StaleReason, "CRM") {
				t.Fatalf("CRM cause missing: %+v", got.Freshness)
			}
		})
	}
}

// TestScopedConversionStaleCRMRetainsVerifiedZeroAndScope 验证旧CRM零值及拒绝范围的可用性不变。
// 参数：t 为隔离回归上下文。
// 返回值：无；真实零值被隐藏或无范围被授予时失败。
func TestScopedConversionStaleCRMRetainsVerifiedZeroAndScope(t *testing.T) {
	repo, _ := conversionFreshnessFixture(t, "FAILED", true, time.Now().UTC().Add(-time.Hour))
	zero, available, err := repo.scopedContract(context.Background(), queryScope{mode: "ORG", tenantID: "t", orgUnitIDs: []string{"empty"}})
	if err != nil || !available || !zero.ConversionAvailable || zero.OpportunityCount != 0 || zero.WonContractCount != 0 || !zero.Freshness.Stale {
		t.Fatalf("verified zero=%+v available=%v err=%v", zero, available, err)
	}
	denied, available, err := repo.scopedContract(context.Background(), queryScope{mode: "NONE", tenantID: "t"})
	if err != nil || available || denied.ConversionAvailable {
		t.Fatalf("missing scope=%+v available=%v err=%v", denied, available, err)
	}
}

// TestMergeScopedConversionFreshnessPreservesContractTimes 验证综合陈旧标记不改写合同时间。
// 参数：t 为隔离回归上下文。
// 返回值：无；时间被其他来源覆盖或已有原因丢失时失败。
func TestMergeScopedConversionFreshnessPreservesContractTimes(t *testing.T) {
	contract := domain.DataFreshness{SourceUpdatedAt: "contract-source", SyncedAt: "contract-sync", ComputedAt: "query", SyncStatus: "SUCCESS"}
	crm := domain.DataFreshness{SyncedAt: "crm-sync", SyncStatus: "FAILED", Stale: true, StaleReason: "最近同步失败"}
	merged := mergeScopedConversionFreshness(contract, crm)
	if !merged.Stale || merged.SyncStatus != "FAILED" || merged.SourceUpdatedAt != contract.SourceUpdatedAt || merged.SyncedAt != contract.SyncedAt || merged.ComputedAt != contract.ComputedAt || !strings.Contains(merged.StaleReason, "CRM最近成功同步：crm-sync") {
		t.Fatalf("merged=%+v", merged)
	}
	contract.Stale = true
	contract.StaleReason = "合同停用"
	merged = mergeScopedConversionFreshness(contract, crm)
	if !strings.Contains(merged.StaleReason, "合同停用") || !strings.Contains(merged.StaleReason, "CRM") {
		t.Fatalf("combined causes=%+v", merged)
	}
	if got := mergeScopedConversionFreshness(contract, domain.DataFreshness{}); got != contract {
		t.Fatalf("fresh dependency changed contract: %+v", got)
	}
}

// TestScopedConversionFreshnessMissingPublicationStaysUnavailable 验证未首次成功不被解释为旧有效零值。
// 参数：t 为隔离回归上下文。
// 返回值：无；来源缺失时未知状态和转化不可用语义不一致则失败。
func TestScopedConversionFreshnessMissingPublicationStaysUnavailable(t *testing.T) {
	repo, db := conversionFreshnessFixture(t, "SUCCESS", true, time.Now().UTC().Add(-time.Hour))
	if err := db.Exec("DELETE FROM sync_source WHERE subsystem_code='customer_and_opportunity'").Error; err != nil {
		t.Fatal(err)
	}
	freshness, err := repo.scopedConversionFreshness(context.Background(), queryScope{tenantID: "t"})
	if err != nil || !freshness.Stale || freshness.SyncStatus != "UNKNOWN" || freshness.SyncedAt != "" {
		t.Fatalf("missing CRM=%+v err=%v", freshness, err)
	}
	_, _, available, err := repo.scopedConversion(context.Background(), queryScope{mode: "ORG", tenantID: "t", orgUnitIDs: []string{"o"}})
	if err != nil || available {
		t.Fatalf("missing publication available=%v err=%v", available, err)
	}
}
