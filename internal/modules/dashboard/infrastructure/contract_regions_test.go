package infrastructure

import (
	"context"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

func regionTestRepo(t *testing.T) *GORMRepository {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE sync_source (id INTEGER PRIMARY KEY, tenant_id TEXT, subsystem_code TEXT, last_run_at TEXT, last_status TEXT, last_success_at TEXT, last_watermark TEXT, enabled INTEGER, updated_at TEXT)`,
		`CREATE TABLE fct_contract_signing (tenant_id TEXT, period_month TEXT, region TEXT, currency TEXT, customer_category TEXT, sales_director_id TEXT, sales_id TEXT, discount_rate_bucket TEXT, sign_amount_minor INTEGER, contract_count INTEGER)`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	return NewGORMRepositoryWithLocation(db, time.UTC)
}

func regionTestContext() context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID:    "tenant-1",
		ScopeMode:   "TENANT",
		Permissions: map[string]struct{}{"dashboard.contract.view": {}},
	})
}

// TestContractRegionsFailsClosedWithoutSuccessfulSync 固定“无成功水位不补零”：
// 即使维表里已有历史行，没有合同来源成功水位时也只能返回不可用说明。
func TestContractRegionsFailsClosedWithoutSuccessfulSync(t *testing.T) {
	repository := regionTestRepo(t)
	if err := repository.db.Exec(`INSERT INTO fct_contract_signing(tenant_id,period_month,region,currency,sign_amount_minor,contract_count) VALUES('tenant-1','2026-11','华东','CNY',100,2)`).Error; err != nil {
		t.Fatal(err)
	}
	distribution, err := repository.ContractRegions(regionTestContext(), "tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	if distribution.Available || distribution.Items == nil || len(distribution.Items) != 0 || distribution.Basis == "" {
		t.Fatalf("expected unavailable empty envelope, got %#v", distribution)
	}
}

// TestContractRegionsAggregatesOnlyAfterSuccessfulSync 固定成功水位后的区域×币种聚合。
func TestContractRegionsAggregatesOnlyAfterSuccessfulSync(t *testing.T) {
	repository := regionTestRepo(t)
	if err := repository.db.Exec(`INSERT INTO sync_source(tenant_id,subsystem_code,last_status,last_success_at,enabled,updated_at) VALUES('tenant-1','contract_management','SUCCESS','2026-11-12 00:00:00',1,'2026-11-12 00:00:00')`).Error; err != nil {
		t.Fatal(err)
	}
	if err := repository.db.Exec(`INSERT INTO fct_contract_signing(tenant_id,period_month,region,currency,sign_amount_minor,contract_count) VALUES('tenant-1','2026-11','华东','CNY',100,2),('tenant-1','2026-11','华南','CNY',50,1)`).Error; err != nil {
		t.Fatal(err)
	}
	distribution, err := repository.ContractRegions(regionTestContext(), "tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	if !distribution.Available || len(distribution.Items) != 2 {
		t.Fatalf("unexpected distribution: %#v", distribution)
	}
	if distribution.Items[0].Region != "华东" || distribution.Items[0].AmountMinor != 100 || distribution.Items[0].ContractCount != 2 {
		t.Fatalf("unexpected first row: %#v", distribution.Items[0])
	}
}
