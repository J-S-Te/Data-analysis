package infrastructure

import (
	"context"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

// regionTestRepo 建立仅含区域统计所需表的隔离内存仓储。
// 参数：t 为当前测试上下文，用于隔离内存数据库名称。
// 返回值：不连接业务数据库的区域统计仓储。
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

// regionTestContext 构造拥有合同读取权限的全租户可信主体。
// 参数：无。
// 返回值：仅用于区域统计单测的请求上下文。
func regionTestContext() context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID:    "tenant-1",
		ScopeMode:   "TENANT",
		Permissions: map[string]struct{}{"dashboard.contract.view": {}},
	})
}

// TestContractRegionsFailsClosedWithoutSuccessfulSync 验证无成功水位时不补零。
// 参数：t 为仅使用内存 SQLite 的仓储测试上下文。
// 返回值：无；无同步凭据仍返回区域数据或缺少不可用说明时失败。
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

// TestContractRegionsAggregatesOnlyAfterSuccessfulSync 验证成功同步后的区域聚合。
// 参数：t 为仅使用内存 SQLite 的仓储测试上下文。
// 返回值：无；区域金额、数量或可用性不符合签约事实时失败。
func TestContractRegionsAggregatesOnlyAfterSuccessfulSync(t *testing.T) {
	repository := regionTestRepo(t)
	if err := repository.db.Exec(`INSERT INTO sync_source(tenant_id,subsystem_code,last_status,last_success_at,enabled,updated_at) VALUES('tenant-1','contract_management','SUCCESS','2026-11-12 00:00:00',1,'2026-11-12 00:00:00')`).Error; err != nil {
		t.Fatal(err)
	}
	if err := repository.db.Exec(`
		INSERT INTO fct_contract_signing
		(tenant_id,period_month,region,currency,sign_amount_minor,contract_count) VALUES
		('tenant-1','2026-11','华东','CNY',100,2),
		('tenant-1','2026-11','华南','CNY',50,1),
		('tenant-1','2026-11','华东','USD',60,1),
		('tenant-2','2026-11','华东','CNY',9999,100)
	`).Error; err != nil {
		t.Fatal(err)
	}
	distribution, err := repository.ContractRegions(regionTestContext(), "tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	if !distribution.Available || len(distribution.Items) != 3 {
		t.Fatalf("unexpected distribution: %#v", distribution)
	}
	if distribution.Items[0].Region != "华东" || distribution.Items[0].AmountMinor != 100 || distribution.Items[0].ContractCount != 2 {
		t.Fatalf("unexpected first row: %#v", distribution.Items[0])
	}
	if distribution.Items[0].Currency != "CNY" || distribution.Items[1].Currency != "USD" ||
		distribution.Items[1].AmountMinor != 60 {
		t.Fatalf("currencies or tenants were merged: %#v", distribution.Items)
	}
}

// TestContractRegionsRejectsNarrowOrUnauthorizedScope 验证区域汇总不会突破可信范围。
// 参数：t 为仅使用内存 SQLite 的仓储测试上下文。
// 返回值：无；窄范围或缺少来源权限时披露区域统计即失败。
func TestContractRegionsRejectsNarrowOrUnauthorizedScope(t *testing.T) {
	repository := regionTestRepo(t)
	if err := repository.db.Exec(`
		INSERT INTO sync_source
		(tenant_id,subsystem_code,last_status,last_success_at,enabled,updated_at) VALUES
		('tenant-1','contract_management','SUCCESS','2026-11-12',1,'2026-11-12')
	`).Error; err != nil {
		t.Fatal(err)
	}
	if err := repository.db.Exec(`
		INSERT INTO fct_contract_signing
		(tenant_id,period_month,region,currency,sign_amount_minor,contract_count) VALUES
		('tenant-1','2026-11','华东','CNY',100,2)
	`).Error; err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"ORG", "TEAM", "SELF", "NONE", "TENANT"} {
		t.Run(mode, func(t *testing.T) {
			principal, _ := auth.FromContext(regionTestContext())
			principal.ScopeMode = mode
			if mode == "TENANT" {
				principal.Permissions = nil
			}
			ctx := auth.WithPrincipal(context.Background(), principal)
			result, err := repository.ContractRegions(ctx, "tenant-1")
			if err != nil || result.Available || len(result.Items) != 0 || result.Basis == "" {
				t.Fatalf("scope or source permission leaked data: %#v, %v", result, err)
			}
		})
	}
}
