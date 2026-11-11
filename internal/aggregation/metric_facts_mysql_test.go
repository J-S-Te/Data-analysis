package aggregation

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/metricfacts"
)

// TestMetricFactsMySQLSourceFailurePreservesPublication 验证机器HTTP经真实迁移和聚合事务发布。
// 参数：t为显式隔离MySQL上下文，不连接或寻找业务数据库。
// 返回值：无；来源失败覆盖旧值、错误推进水位或范围泄露时失败。
func TestMetricFactsMySQLSourceFailurePreservesPublication(t *testing.T) {
	db, _ := dashboardAcceptanceDB(t, "DASHBOARD_METRIC_FACTS_TEST_DSN_FILE", true)
	tenant := "metrics-http-mysql"
	for _, table := range []string{"dashboard_metric_snapshot", "sync_source", "api_contract_dashboard", "sync_run"} {
		if err := db.Exec("DELETE FROM "+table+" WHERE tenant_id=?", tenant).Error; err != nil {
			t.Fatal(err)
		}
	}
	metricStatus := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer synthetic-metrics-token" || req.Header.Get("X-DA-Tenant-ID") != tenant {
			t.Error("missing fixture machine identity")
		}
		if req.URL.Path == "/internal/v1/dashboard/metrics" {
			w.WriteHeader(metricStatus)
			_, _ = fmt.Fprintf(w, `{"code":"OK","data":{"schema_version":1,"tenant_id":%q,"source_code":"contract_management","snapshot_at":%q,"complete":true,"basis":"isolated frozen prices","rows":[{"kind":"discount","record_id":"contract-a","organization_id":"org-a","identity_id":"u1","project_id":"","period":"2026-11","status":"KNOWN","currency":"CNY","amount_minor":8000,"standard_minor":10000,"bucket":"FROM_80_TO_90"}]}}`, tenant, time.Now().UTC().Format(time.RFC3339Nano))
			return
		}
		_, _ = fmt.Fprintf(w, `{"code":"OK","data":{"tenant_id":%q,"total_amount_minor":8000,"total_contracts":1,"currency_totals":[{"currency":"CNY","amount_minor":8000,"contracts":1}],"approval_contracts":0,"active_contracts":1,"expired_contracts":0}}`, tenant)
	}))
	defer server.Close()
	api := NewAPISyncRunner(db, APISyncOptions{TenantID: tenant, MetricFactsEnabled: true,
		ContractInternalURL: server.URL + "/internal/v1/dashboard",
		ContractCredential:  MachineCredential{ClientID: "metrics-http", Scope: "dashboard.contract.read"}})
	api.tokens["metrics-http"] = cachedMachineToken{value: "synthetic-metrics-token", expiresAt: time.Now().Add(time.Hour)}
	r := &Runner{aggDB: db, tenantID: tenant, ProjectAPI: api}
	if err := r.EnsureSyncSources(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	store := &metricfacts.Store{DB: db}
	before, err := store.Read(context.Background(), tenant, "contract", "", metricfacts.Scope{Mode: "TENANT"})
	if err != nil || before.DiscountKnown != 1 || before.Stale {
		t.Fatalf("before=%+v err=%v", before, err)
	}
	metricStatus = http.StatusServiceUnavailable
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatal("dependency failure concealed")
	}
	after, err := store.Read(context.Background(), tenant, "contract", "", metricfacts.Scope{Mode: "TENANT"})
	if err != nil || !after.Available || !after.Stale || after.SnapshotAt != before.SnapshotAt || after.DiscountKnown != 1 {
		t.Fatalf("after=%+v err=%v", after, err)
	}
	narrow, err := store.Read(context.Background(), tenant, "contract", "", metricfacts.Scope{Mode: "ORG", OrganizationIDs: []string{"org-b"}})
	if err != nil || narrow.DiscountKnown != 0 {
		t.Fatalf("scope leaked %+v %v", narrow, err)
	}
}
