package infrastructure

import (
	"context"
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/alerts/application"
)

// TestUpdateStatusRecordsManualCloseReason 验证人工关闭写入 MANUAL，规则自动收敛
// 因此不会重开人工决定；ACK 会清理关闭原因。
// 参数：t 为测试上下文。
// 返回值：无。
func TestUpdateStatusRecordsManualCloseReason(t *testing.T) {
	db := alertScopeTestDB(t)
	ctx := context.Background()
	if err := db.Exec(`INSERT INTO alert_item
(id, tenant_id, alert_type, rule_code, severity, owner_org_id, target_ref, title, status, created_at, updated_at)
VALUES ('a', 'tenant-1', 'CONTRACT_EXPIRY', 'CONTRACT_EXPIRY', 'HIGH', 'org-a', 'contract-a', 'A', 'ACK', ?, ?)`,
		time.Now().UTC(), time.Now().UTC()).Error; err != nil {
		t.Fatal(err)
	}
	repository := NewGORMRepository(db)
	scope := application.AlertScope{Mode: "TENANT"}
	now := time.Now().UTC()
	if changed, err := repository.UpdateStatus(ctx, "a", "tenant-1", "CLOSED", scope, now, &now); err != nil || !changed {
		t.Fatalf("人工关闭失败 changed=%v err=%v", changed, err)
	}
	var row alertRecord
	if err := db.Where("id = ?", "a").Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != "CLOSED" || row.ClosedReason != "MANUAL" || row.ClosedAt == nil {
		t.Fatalf("人工关闭未记录原因 %+v", row)
	}
	if changed, err := repository.UpdateStatus(ctx, "a", "tenant-1", "ACK", scope, now, nil); err != nil || !changed {
		t.Fatalf("重新确认失败 changed=%v err=%v", changed, err)
	}
	if err := db.Where("id = ?", "a").Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != "ACK" || row.ClosedReason != "" || row.ClosedAt != nil {
		t.Fatalf("重新确认未清理关闭证据 %+v", row)
	}
}
