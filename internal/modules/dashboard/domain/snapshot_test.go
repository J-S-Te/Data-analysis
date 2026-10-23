package domain

import (
	"encoding/json"
	"testing"
)

// TestSnapshotJSONUsesAPIFieldNames 防止领域快照序列化为 Go 默认的大驼峰字段，
// 确保前端约定的 snake_case 字段始终稳定。
func TestSnapshotJSONUsesAPIFieldNames(t *testing.T) {
	contract, err := json.Marshal(ContractSnapshot{Available: true, TenantID: "tenant-1", TotalContracts: 2})
	if err != nil {
		t.Fatalf("marshal contract snapshot: %v", err)
	}
	if got := string(contract); got != `{"available":true,"tenant_id":"tenant-1","snapshot_at":"","freshness":{"source_updated_at":"","synced_at":"","computed_at":"","sync_status":"","stale":false,"stale_reason":""},"total_amount_minor":0,"currency_totals":null,"total_contracts":2,"approval_contracts":0,"active_contracts":0,"expired_contracts":0,"opportunity_count":0,"won_contract_count":0,"conversion_available":false,"conversion_basis":"","discount_available":false,"discount_unavailable_reason":"","discount_buckets":null}` {
		t.Fatalf("unexpected contract JSON: %s", got)
	}

	project, err := json.Marshal(ProjectSnapshot{Available: true, ProjectCount: 3, StatusCounts: map[string]int{"ACTIVE": 3}})
	if err != nil {
		t.Fatalf("marshal project snapshot: %v", err)
	}
	if got := string(project); got != `{"available":true,"tenant_id":"","snapshot_at":"","freshness":{"source_updated_at":"","synced_at":"","computed_at":"","sync_status":"","stale":false,"stale_reason":""},"project_count":3,"in_flight_projects":0,"risk_projects":0,"service_items":0,"status_counts":{"ACTIVE":3}}` {
		t.Fatalf("unexpected project JSON: %s", got)
	}
}
