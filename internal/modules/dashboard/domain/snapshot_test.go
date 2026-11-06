package domain

import (
	"encoding/json"
	"strings"
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
	if got := string(project); got != `{"available":true,"tenant_id":"","snapshot_at":"","freshness":{"source_updated_at":"","synced_at":"","computed_at":"","sync_status":"","stale":false,"stale_reason":""},"project_count":3,"in_flight_projects":0,"risk_projects":0,"service_items":0,"status_counts":{"ACTIVE":3},"report_stage_counts":null}` {
		t.Fatalf("unexpected project JSON: %s", got)
	}
}

// TestProjectSnapshotReportStageCountsDistinguishesNotCollected 固定“未采集”与
// “已采集且无分桶”的序列化差异：nil 必须是 null，空映射必须是 {}，否则调用方
// 无法区分“旧数据未采集”和“0 个整改中”。
func TestProjectSnapshotReportStageCountsDistinguishesNotCollected(t *testing.T) {
	notCollected, err := json.Marshal(ProjectSnapshot{StatusCounts: map[string]int{}})
	if err != nil {
		t.Fatalf("marshal project snapshot: %v", err)
	}
	if !strings.Contains(string(notCollected), `"report_stage_counts":null`) {
		t.Fatalf("uncollected counts JSON: %s", notCollected)
	}
	collected, err := json.Marshal(ProjectSnapshot{
		StatusCounts:      map[string]int{},
		ReportStageCounts: map[string]int{},
	})
	if err != nil {
		t.Fatalf("marshal project snapshot: %v", err)
	}
	if !strings.Contains(string(collected), `"report_stage_counts":{}`) {
		t.Fatalf("collected empty counts JSON: %s", collected)
	}
	withValues, err := json.Marshal(ProjectSnapshot{
		StatusCounts:      map[string]int{},
		ReportStageCounts: map[string]int{"RECTIFYING": 2, "": 1},
	})
	if err != nil {
		t.Fatalf("marshal project snapshot: %v", err)
	}
	for _, fragment := range []string{`"report_stage_counts":{`, `"RECTIFYING":2`, `"":1`} {
		if !strings.Contains(string(withValues), fragment) {
			t.Fatalf("counts JSON %s missing %s", withValues, fragment)
		}
	}
}
