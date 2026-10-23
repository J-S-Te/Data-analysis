package auditoutbox

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newReclaimTestStore(t *testing.T, maxTries uint) *GORMStore {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "audit.sqlite")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&outboxRow{}); err != nil {
		t.Fatal(err)
	}
	return &GORMStore{DB: db, Lease: 2 * time.Minute, MaxTries: maxTries}
}

// 真实 pending -> processing 认领后崩溃，不得搁置
// 最后一次尝试。恢复保留事件，并记录真实投递结果。
func TestClaimReclaimsCrashOnFinalAttempt(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC)
	store := newReclaimTestStore(t, 3)
	row := outboxRow{ID: "crash", TenantID: "tenant-1", EventID: "immutable-event", Action: "AUDIT", Result: "SUCCESS", DeliveryStatus: "pending", Attempts: 2, NextAttemptAt: now}
	if err := store.DB.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	first, err := store.Claim(ctx, "worker-a", 1, now)
	if err != nil || len(first) != 1 || first[0].Attempts != 3 {
		t.Fatalf("initial claim: %+v %v", first, err)
	}
	active, err := store.Claim(ctx, "worker-b", 1, now.Add(time.Minute))
	if err != nil || len(active) != 0 {
		t.Fatalf("active final-attempt lease stolen: %+v %v", active, err)
	}
	recovered, err := store.Claim(ctx, "worker-b", 1, now.Add(3*time.Minute))
	if err != nil || len(recovered) != 1 || recovered[0].EventID != "immutable-event" || recovered[0].Attempts != 4 {
		t.Fatalf("expired final-attempt processing lost: %+v %v", recovered, err)
	}
	if err := store.DB.First(&row, "id = ?", "crash").Error; err != nil || row.DeliveryStatus != "processing" || row.DeliveredAt != nil {
		t.Fatalf("recovery must not fake delivery: %+v %v", row, err)
	}
	if err := store.MarkFailed(ctx, "crash", "PLATFORM_AUDIT_UNAVAILABLE", now.Add(4*time.Minute), now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var failed outboxRow
	if err := store.DB.First(&failed, "id = ?", "crash").Error; err != nil || failed.DeliveryStatus != "failed" || failed.LockedAt != nil || failed.DeliveredAt != nil || deref(failed.LastError) != "PLATFORM_AUDIT_UNAVAILABLE" {
		t.Fatalf("failed recovery outcome lost: %+v %v", failed, err)
	}
	entries, err := store.Claim(ctx, "worker-c", 1, now.Add(time.Hour))
	if err != nil || len(entries) != 0 {
		t.Fatalf("terminal failure auto-retried: %+v %v", entries, err)
	}
}

func TestClaimOnlyTakesDueUnleasedOrExpiredWork(t *testing.T) {
	now := time.Date(2026, 10, 20, 2, 0, 0, 0, time.UTC)
	lock := func(age time.Duration) *time.Time { value := now.Add(-age); return &value }
	for _, tc := range []struct {
		name     string
		maxTries uint
	}{{"unlimited", 0}, {"limited", 3}} {
		t.Run(tc.name, func(t *testing.T) {
			store := newReclaimTestStore(t, tc.maxTries)
			rows := []outboxRow{
				{ID: "pending", DeliveryStatus: "pending", Attempts: 0, NextAttemptAt: now},
				{ID: "retry", DeliveryStatus: "retry", Attempts: 1, NextAttemptAt: now},
				{ID: "expired", DeliveryStatus: "processing", Attempts: 1, LockedAt: lock(3 * time.Minute), NextAttemptAt: now},
				{ID: "active", DeliveryStatus: "processing", Attempts: 1, LockedAt: lock(time.Minute), NextAttemptAt: now},
				{ID: "boundary", DeliveryStatus: "processing", Attempts: 1, LockedAt: lock(2 * time.Minute), NextAttemptAt: now},
				{ID: "future", DeliveryStatus: "processing", Attempts: 1, LockedAt: lock(3 * time.Minute), NextAttemptAt: now.Add(time.Hour)},
				{ID: "failed", DeliveryStatus: "failed", Attempts: 1, LockedAt: lock(3 * time.Minute), NextAttemptAt: now},
				{ID: "delivered", DeliveryStatus: "delivered", Attempts: 1, NextAttemptAt: now},
			}
			for i := range rows {
				rows[i].TenantID, rows[i].EventID = "tenant-1", "event-"+rows[i].ID
			}
			if err := store.DB.Create(&rows).Error; err != nil {
				t.Fatal(err)
			}
			entries, err := store.Claim(context.Background(), "worker", 20, now)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]bool{}
			for _, entry := range entries {
				got[entry.ID] = true
			}
			if len(entries) != 3 || !got["pending"] || !got["retry"] || !got["expired"] {
				t.Fatalf("wrong eligibility: %+v", entries)
			}
			for _, id := range []string{"active", "boundary", "future", "failed", "delivered"} {
				var row outboxRow
				if err := store.DB.First(&row, "id = ?", id).Error; err != nil || row.Attempts != 1 {
					t.Fatalf("ineligible row mutated: %+v %v", row, err)
				}
			}
		})
	}
}

func TestAuditFailureStillHonorsRetryBudgetAndBackoff(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	store := newReclaimTestStore(t, 2)
	rows := []outboxRow{
		{ID: "ordinary", DeliveryStatus: "pending", NextAttemptAt: now},
		{ID: "maxed-pending", DeliveryStatus: "pending", Attempts: 2, NextAttemptAt: now},
		{ID: "maxed-retry", DeliveryStatus: "retry", Attempts: 2, NextAttemptAt: now},
	}
	if err := store.DB.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	entries, err := store.Claim(ctx, "worker", 20, now)
	if err != nil || len(entries) != 1 || entries[0].ID != "ordinary" || entries[0].Attempts != 1 {
		t.Fatalf("maxed pending/retry claimed: %+v %v", entries, err)
	}
	if err := store.MarkFailed(ctx, "ordinary", "DOWN", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	entries, err = store.Claim(ctx, "worker", 20, now.Add(30*time.Second))
	if err != nil || len(entries) != 0 {
		t.Fatalf("retry ignored backoff: %+v %v", entries, err)
	}
	entries, err = store.Claim(ctx, "worker", 20, now.Add(time.Minute))
	if err != nil || len(entries) != 1 || entries[0].Attempts != 2 {
		t.Fatalf("due retry not delivered: %+v %v", entries, err)
	}
	if err := store.MarkFailed(ctx, "ordinary", "DOWN", now.Add(2*time.Minute), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var row outboxRow
	if err := store.DB.First(&row, "id = ?", "ordinary").Error; err != nil || row.DeliveryStatus != "failed" || row.DeliveredAt != nil {
		t.Fatalf("retry cap changed: %+v %v", row, err)
	}
}
