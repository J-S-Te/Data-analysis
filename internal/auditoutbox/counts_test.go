package auditoutbox

import (
	"context"
	"testing"
	"time"
)

// C2：outbox 三态必须分别可观测——pending/retry/processing 是积压（还能自愈），
// failed 是死亡事件（只能人工重放），delivered 是已投递。混在一起会让告警失灵。
func TestCountsSeparatesPendingDeliveredAndDead(t *testing.T) {
	store := newReclaimTestStore(t, 3)
	now := time.Date(2026, 11, 6, 2, 0, 0, 0, time.UTC)
	rows := []outboxRow{
		{ID: "p1", DeliveryStatus: "pending", NextAttemptAt: now},
		{ID: "p2", DeliveryStatus: "pending", NextAttemptAt: now},
		{ID: "r1", DeliveryStatus: "retry", Attempts: 1, NextAttemptAt: now},
		{ID: "processing", DeliveryStatus: "processing", Attempts: 1, LockedAt: &now, NextAttemptAt: now},
		{ID: "d1", DeliveryStatus: "delivered", Attempts: 1, NextAttemptAt: now},
		{ID: "f1", DeliveryStatus: "failed", Attempts: 20, NextAttemptAt: now},
	}
	for i := range rows {
		rows[i].TenantID, rows[i].EventID = "tenant-1", "event-"+rows[i].ID
	}
	if err := store.DB.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}

	counts, err := store.Counts(context.Background())
	if err != nil {
		t.Fatalf("Counts() error = %v", err)
	}
	if counts.Pending != 4 || counts.Delivered != 1 || counts.Dead != 1 {
		t.Fatalf("Counts() = %+v, want pending=4 delivered=1 dead=1", counts)
	}
}

// 空表（首次部署、审计关闭）必须是全零而不是错误：抓取端要能看到“确实没有积压”。
func TestCountsOnEmptyOutboxIsZero(t *testing.T) {
	store := newReclaimTestStore(t, 20)
	counts, err := store.Counts(context.Background())
	if err != nil {
		t.Fatalf("Counts() error = %v", err)
	}
	if counts != (OutboxCounts{}) {
		t.Fatalf("Counts() = %+v, want zero", counts)
	}
}

// 未初始化的 store 必须报错，不能伪装成“没有积压”。
func TestCountsRequiresInitializedStore(t *testing.T) {
	var store *GORMStore
	if _, err := store.Counts(context.Background()); err == nil {
		t.Fatal("Counts() on nil store = nil error, want error")
	}
	if _, err := (&GORMStore{}).Counts(context.Background()); err == nil {
		t.Fatal("Counts() without DB = nil error, want error")
	}
}
