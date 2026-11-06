package standardfacts

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/unified-identity-auth-platform/data-analysis/internal/migration"
	"github.com/unified-identity-auth-platform/data-analysis/migrations"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// isolatedFinanceStore 仅连接明确指定且经空库检查的标准事实测试库。
// 参数：t为测试上下文；DSN来自STANDARD_FINANCE_TEST_DSN且库名必须有专用前缀。
// 返回值：已连续执行两次真实迁移的仓储；未配置时跳过且不连接。
func isolatedFinanceStore(t *testing.T) *Store {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("STANDARD_FINANCE_TEST_DSN"))
	if dsn == "" {
		t.Skip("requires dedicated empty STANDARD_FINANCE_TEST_DSN; no connection attempted")
	}
	config, err := driver.ParseDSN(dsn)
	if err != nil || !strings.HasPrefix(config.DBName, "standard_finance_test_") {
		t.Fatal("requires dedicated standard_finance_test_ schema")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("open isolated finance database failed")
	}
	pool, _ := db.DB()
	t.Cleanup(func() { _ = pool.Close() })
	var tables int64
	if err := db.Raw("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()").Scan(&tables).Error; err != nil || tables != 0 {
		t.Fatal("requires empty isolated schema")
	}
	for range 2 {
		if _, err := migration.Run(context.Background(), dsn, migrations.Files); err != nil {
			t.Fatal(err)
		}
	}
	return &Store{DB: db}
}

// TestStandardFinanceConcurrentMySQL 核验真实MySQL发布锁、幂等、版本和事实计数。
// 参数：t为测试上下文，数据仅写入通过空库门禁的专用测试库。
// 返回值：无；同批重试重复事实、版本回退或指针漂移时失败。
func TestStandardFinanceConcurrentMySQL(t *testing.T) {
	store := isolatedFinanceStore(t)
	ctx, now := context.Background(), time.Now()
	document := financeFixture()
	preview, err := store.Preview(ctx, "finance-concurrency-test", document, now)
	if err != nil {
		t.Fatal(err)
	}
	request := PublishRequest{Document: document, Digest: preview.Digest, ExpectedVersion: 0}
	results := make(chan Publication, 8)
	failures := make(chan error, 8)
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			result, err := store.Publish(ctx, "finance-concurrency-test", "synthetic-actor", "", request, now)
			if err != nil {
				failures <- err
			} else {
				results <- result
			}
		}()
	}
	workers.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	created := 0
	for result := range results {
		if !result.Published || result.Version != 1 || result.BatchID != document.BatchID {
			t.Errorf("unexpected publication: version=%d published=%t", result.Version, result.Published)
		}
		if !result.Replayed {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("initial publications=%d, want 1", created)
	}
	assertFinanceMySQLState(t, store, request, now)
}

// assertFinanceMySQLState 核验发布后的持久事实与冲突拒绝保持同一版本。
// 参数：t为测试上下文，store为专用库仓储，request为已发布请求，now为校验时刻。
// 返回值：无；事实、审计重复或旧版本替换成功时失败。
func assertFinanceMySQLState(t *testing.T, store *Store, request PublishRequest, now time.Time) {
	t.Helper()
	for table, want := range map[string]int64{
		"standard_finance_source": 1, "standard_finance_batch": 1,
		"standard_finance_fact": 4, "bi_audit_outbox": 1,
	} {
		var count int64
		if err := store.DB.Table(table).Where("tenant_id=?", "finance-concurrency-test").Count(&count).Error; err != nil || count != want {
			t.Fatalf("%s: count=%d want=%d error=%v", table, count, want, err)
		}
	}
	request.Document.BatchID = "new-batch-old-version"
	changed, _ := Analyze(request.Document, now)
	request.Digest = changed.Digest
	if _, err := store.Publish(context.Background(), "finance-concurrency-test", "synthetic-actor", "", request, now); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("old version replacement: %v", err)
	}
	state, err := store.Status(context.Background(), "finance-concurrency-test")
	if err != nil || state.Version != 1 || state.BatchID == request.Document.BatchID {
		t.Fatalf("pointer changed after rejected replacement: version=%d error=%v", state.Version, err)
	}
}
