package standardfacts

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// factsStoreFixture 建立隔离内存库，仅验证本模块持久化边界。
// 参数：t 为测试上下文。
// 返回值：真实仓储及只属于本测试的数据库连接。
func factsStoreFixture(t *testing.T) (*Store, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := db.DB()
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.AutoMigrate(&sourceRow{}, &batchRow{}, &factRow{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE bi_audit_outbox (id TEXT, tenant_id TEXT, event_id TEXT, actor_id TEXT, action TEXT, resource_type TEXT, resource_id TEXT, request_id TEXT, login_ip BLOB, client_ip BLOB, ip_source TEXT, result TEXT, delivery_status TEXT, attempts INTEGER, next_attempt_at DATETIME, created_at DATETIME, payload TEXT)`).Error; err != nil {
		t.Fatal(err)
	}
	return &Store{DB: db}, db
}

// TestPublishAtomicIdempotentTenantFacts 验证发布真实指标、版本冲突、批次重放和租户隔离。
// 参数：t 为测试上下文。
// 返回值：无。
func TestPublishAtomicIdempotentTenantFacts(t *testing.T) {
	store, db := factsStoreFixture(t)
	ctx := context.Background()
	now := time.Now()
	before, err := store.Read(ctx, "tenant-a")
	if err != nil || before.Available {
		t.Fatalf("unpublished is available: %+v %v", before, err)
	}
	doc := financeFixture()
	preview, err := store.Preview(ctx, "tenant-a", doc, now)
	if err != nil || preview.ExpectedVersion != 0 {
		t.Fatalf("preview %+v %v", preview, err)
	}
	request := PublishRequest{Document: doc, Digest: preview.Digest, ExpectedVersion: 0}
	published, err := store.Publish(ctx, "tenant-a", "actor", "", request, now)
	if err != nil || published.Version != 1 {
		t.Fatalf("publish %+v %v", published, err)
	}
	replay, err := store.Publish(ctx, "tenant-a", "actor", "", request, now)
	if err != nil || !replay.Replayed || replay.Version != 1 {
		t.Fatalf("replay %+v %v", replay, err)
	}
	var count int64
	db.Model(&factRow{}).Count(&count)
	if count != 4 {
		t.Fatalf("duplicate facts=%d", count)
	}
	db.Table("bi_audit_outbox").Count(&count)
	if count != 1 {
		t.Fatalf("audit count=%d", count)
	}
	doc.Receivables[0].OriginalMinor++
	changed, _ := Analyze(doc, now)
	request.Document = doc
	request.Digest = changed.Digest
	if _, err = store.Publish(ctx, "tenant-a", "actor", "", request, now); !errors.Is(err, ErrBatchConflict) {
		t.Fatalf("batch mismatch accepted: %v", err)
	}
	request.Document.BatchID = "batch-002"
	changed, _ = Analyze(request.Document, now)
	request.Digest = changed.Digest
	if _, err = store.Publish(ctx, "tenant-a", "actor", "", request, now); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale version accepted: %v", err)
	}
	current, err := store.Read(ctx, "tenant-a")
	if err != nil || !current.Available || current.TenantID != "tenant-a" {
		t.Fatalf("read %+v %v", current, err)
	}
	other, err := store.Read(ctx, "tenant-b")
	if err != nil || other.Available {
		t.Fatalf("cross-tenant leak: %+v %v", other, err)
	}
	request.ExpectedVersion = 1
	if _, err = store.Publish(ctx, "tenant-a", "actor", "", request, now); err != nil {
		t.Fatal(err)
	}
	status, _ := store.Status(ctx, "tenant-a")
	if status.Version != 2 || status.BatchID != "batch-002" {
		t.Fatalf("pointer %+v", status)
	}
}

// TestPublishFailureRollsBackFactsAndPointer 验证审计失败时事实与发布指针同事务回滚。
// 参数：t 为测试上下文。
// 返回值：无。
func TestPublishFailureRollsBackFactsAndPointer(t *testing.T) {
	store, db := factsStoreFixture(t)
	ctx := context.Background()
	doc := financeFixture()
	now := time.Now()
	preview, _ := Analyze(doc, now)
	db.Exec("DROP TABLE bi_audit_outbox")
	if _, err := store.Publish(ctx, "tenant", "actor", "", PublishRequest{Document: doc, Digest: preview.Digest}, now); err == nil {
		t.Fatal("publish without audit succeeded")
	}
	var facts int64
	db.Model(&factRow{}).Count(&facts)
	if facts != 0 {
		t.Fatalf("partial facts=%d", facts)
	}
	status, err := store.Status(ctx, "tenant")
	if err != nil || status.Version != 0 {
		t.Fatalf("partial pointer %+v %v", status, err)
	}
}

// TestPublishRejectsLicenseAndCorruption 验证重验许可与损坏指针均不伪装成功或未发布。
// 参数：t 为测试上下文。
// 返回值：无。
func TestPublishRejectsLicenseAndCorruption(t *testing.T) {
	store, db := factsStoreFixture(t)
	ctx := context.Background()
	doc := financeFixture()
	now := time.Now()
	preview, _ := Analyze(doc, now)
	request := PublishRequest{Document: doc, Digest: preview.Digest}
	store.LicenseCheck = func(context.Context, string) error { return errors.New("expired") }
	if _, err := store.Publish(ctx, "tenant", "actor", "", request, now); !errors.Is(err, ErrLicenseDenied) {
		t.Fatalf("license not enforced: %v", err)
	}
	var count int64
	db.Model(&batchRow{}).Count(&count)
	if count != 0 {
		t.Fatal("license denial wrote batch")
	}
	store.LicenseCheck = nil
	request.Digest = "changed"
	if _, err := store.Publish(ctx, "tenant", "actor", "", request, now); !errors.Is(err, ErrInvalidPreview) {
		t.Fatalf("wrong digest accepted: %v", err)
	}
	db.Create(&sourceRow{TenantID: "tenant", SourceCode: SourceCode, Version: 1, BatchID: "missing"})
	if _, err := store.Read(ctx, "tenant"); err == nil {
		t.Fatal("corrupt pointer hidden as unpublished")
	}
	db.Exec("DROP TABLE standard_finance_source")
	if _, err := store.Status(ctx, "tenant"); err == nil {
		t.Fatal("storage failure hidden as unpublished")
	}
}
