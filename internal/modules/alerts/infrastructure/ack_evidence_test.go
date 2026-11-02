package infrastructure

import (
	"context"
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/alerts/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

// TestUpdateStatusRecordsAcknowledgedEvidence 验证人工确认（ACK）写入
// acknowledged_at/acknowledged_by，关闭（CLOSED）不清空已有确认证据，规则复开
// 依赖的 closed_reason 口径保持 MANUAL。
// 参数：t 为测试上下文。
// 返回值：无。
func TestUpdateStatusRecordsAcknowledgedEvidence(t *testing.T) {
	db := alertScopeTestDB(t)
	now := time.Now().UTC()
	if err := db.Exec(`INSERT INTO alert_item
(id, tenant_id, alert_type, rule_code, severity, owner_org_id, target_ref, title, status, created_at, updated_at)
VALUES ('ack-1', 'tenant-1', 'CONTRACT_EXPIRY', 'CONTRACT_EXPIRY', 'HIGH', 'org-a', 'contract-a', 'A', 'OPEN', ?, ?)`,
		now, now).Error; err != nil {
		t.Fatal(err)
	}
	repository := NewGORMRepository(db)
	scope := application.AlertScope{Mode: "TENANT"}
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		UserID: "user-7", Username: "seven", TenantID: "tenant-1",
	})

	if changed, err := repository.UpdateStatus(ctx, "ack-1", "tenant-1", "ACK", scope, now, nil); err != nil || !changed {
		t.Fatalf("人工确认失败 changed=%v err=%v", changed, err)
	}
	var row alertRecord
	if err := db.Where("id = ?", "ack-1").Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != "ACK" || row.AcknowledgedAt == nil || row.AcknowledgedBy != "user-7" {
		t.Fatalf("人工确认未记录确认证据 %+v", row)
	}

	// 人工关闭只追加关闭原因，确认证据保留供追溯。
	closedAt := now.Add(time.Minute)
	if changed, err := repository.UpdateStatus(ctx, "ack-1", "tenant-1", "CLOSED", scope, closedAt, &closedAt); err != nil || !changed {
		t.Fatalf("人工关闭失败 changed=%v err=%v", changed, err)
	}
	if err := db.Where("id = ?", "ack-1").Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != "CLOSED" || row.ClosedReason != "MANUAL" {
		t.Fatalf("人工关闭未记录 MANUAL %+v", row)
	}
	if row.AcknowledgedAt == nil || row.AcknowledgedBy != "user-7" {
		t.Fatalf("关闭清空了确认证据 %+v", row)
	}
}

// TestUpdateStatusAckWithoutPrincipalLeavesActorEmpty 验证无认证主体（如内部批处理）
// 时只写确认时间、不伪造操作者。
// 参数：t 为测试上下文。
// 返回值：无。
func TestUpdateStatusAckWithoutPrincipalLeavesActorEmpty(t *testing.T) {
	db := alertScopeTestDB(t)
	now := time.Now().UTC()
	if err := db.Exec(`INSERT INTO alert_item
(id, tenant_id, alert_type, rule_code, severity, owner_org_id, target_ref, title, status, created_at, updated_at)
VALUES ('ack-2', 'tenant-1', 'CONTRACT_EXPIRY', 'CONTRACT_EXPIRY', 'HIGH', 'org-a', 'contract-b', 'B', 'OPEN', ?, ?)`,
		now, now).Error; err != nil {
		t.Fatal(err)
	}
	repository := NewGORMRepository(db)
	scope := application.AlertScope{Mode: "TENANT"}
	if changed, err := repository.UpdateStatus(context.Background(), "ack-2", "tenant-1", "ACK", scope, now, nil); err != nil || !changed {
		t.Fatalf("人工确认失败 changed=%v err=%v", changed, err)
	}
	var row alertRecord
	if err := db.Where("id = ?", "ack-2").Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.AcknowledgedAt == nil || row.AcknowledgedBy != "" {
		t.Fatalf("无主体时确认证据不正确 %+v", row)
	}
}
