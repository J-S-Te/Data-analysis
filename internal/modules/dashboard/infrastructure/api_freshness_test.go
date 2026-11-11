package infrastructure

import (
	"context"
	"testing"
	"time"
)

// TestAPIDashboardFailedSourceRetainsSnapshotWatermark 验证接口同步失败立即展示陈旧且保留数据时间。
// 参数：t 为独立内存数据库测试上下文。
// 返回值：无；不能用最近一次失败或其它通道的成功时间冒充快照时间。
func TestAPIDashboardFailedSourceRetainsSnapshotWatermark(t *testing.T) {
	db := freshnessTestDB(t)
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second).Format(time.RFC3339)
	if err := db.Exec(`INSERT INTO api_project_dashboard
		(tenant_id,snapshot_at,status_counts_json,report_stage_counts_json)
		VALUES ('t',?,'{}','{}')`, at).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO sync_source
		(id,tenant_id,subsystem_code,enabled,last_status,last_success_at,updated_at)
		VALUES ('s','t','project_dashboard',1,'FAILED',?,?)`, time.Now(), time.Now()).Error; err != nil {
		t.Fatal(err)
	}
	value, available, err := NewGORMRepositoryWithLocation(db, time.UTC).LatestProject(context.Background(), "t")
	if err != nil || !available || !value.Freshness.Stale || value.Freshness.SyncedAt != at ||
		value.Freshness.SyncStatus != "FAILED" {
		t.Fatalf("API失败元数据错误: %+v available=%v err=%v", value, available, err)
	}
}
