package aggregation

import (
	"context"
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/migration"
	"github.com/unified-identity-auth-platform/data-analysis/migrations"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// watermarkPrecisionMySQL 打开显式配置的回环隔离库并应用真实迁移。
// 参数：t 为测试上下文，仅接受私有DSN文件和data_analysis_test_*_test测试schema。
// 返回值：真实MySQL连接；无DSN跳过，隔离约束、迁移或连接异常终止测试。
func watermarkPrecisionMySQL(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := importPrivateDSN(t, "UIP_BI_WATERMARK_PRECISION_TEST_DSN_FILE")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := migration.Run(ctx, dsn, migrations.Files); err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("open isolated watermark database failed")
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	return db
}

// watermarkPrecisionSource 插入本用例独占来源，避免其他用例或租户影响断言。
// 参数：t/db 为测试及连接，name 为场景标识，success 为预存最近成功凭据。
// 返回值：隔离执行器及来源行ID；插入失败终止测试，结束时仅清理本用例行。
func watermarkPrecisionSource(
	t *testing.T, db *gorm.DB, name string, success *time.Time,
) (*Runner, string) {
	t.Helper()
	tenant := "watermark-precision"
	id := ulid()
	source := SyncSource{
		TenantID: tenant, ID: id, SubsystemCode: name,
		DBHost: "isolated", DBSchema: "isolated", ReadAccountRef: "isolated.read",
		Enabled: true, LastStatus: "PENDING", LastSuccessAt: success,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := db.Create(&source).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Where("tenant_id=? AND id=?", tenant, id).
			Delete(&SyncSource{}).Error; err != nil {
			t.Error(err)
		}
	})
	return &Runner{aggDB: db, tenantID: tenant}, id
}

// watermarkPrecisionRead 读取当前测试来源的真实持久状态。
// 参数：t/db 为测试及连接，id 为本用例独占来源行ID。
// 返回值：数据库来源行；读取失败终止测试。
func watermarkPrecisionRead(t *testing.T, db *gorm.DB, id string) SyncSource {
	t.Helper()
	var source SyncSource
	if err := db.Where("id=?", id).First(&source).Error; err != nil {
		t.Fatal(err)
	}
	return source
}

// TestWatermarkPrecisionMySQLRunningTerminal 验证跨秒精度不会使同轮终态滞留RUNNING。
// 参数：t 为测试上下文，环境变量指定新建且明确归属的本机MySQL8.4测试库。
// 返回值：无；成功/失败终态丢失、失败清空成功凭据或时区改变语义时失败。
func TestWatermarkPrecisionMySQLRunningTerminal(t *testing.T) {
	db := watermarkPrecisionMySQL(t)
	ctx := context.Background()
	previous := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	locations := []*time.Location{time.UTC, time.FixedZone("UTC+8", 8*3600)}
	for _, status := range []string{statusSuccess, statusFailed} {
		for _, location := range locations {
			t.Run(status+"/"+location.String(), func(t *testing.T) {
				code := "precision-" + status + "-" + location.String()
				r, id := watermarkPrecisionSource(t, db, code, &previous)
				runAt := time.Date(2026, 11, 11, 12, 34, 56, 999600000, time.UTC).
					In(location)
				r.mark(ctx, code, statusRunning, runAt, nil)
				running := watermarkPrecisionRead(t, db, id)
				if running.LastRunAt == nil || running.LastStatus != statusRunning {
					t.Fatalf("RUNNING was not persisted: %+v", running)
				}
				t.Logf("runAt=%s persisted-running=%s",
					runAt.Format(time.RFC3339Nano),
					running.LastRunAt.Format(time.RFC3339Nano))

				var message *string
				if status == statusFailed {
					value := "synthetic source failure"
					message = &value
				}
				r.mark(ctx, code, status, runAt, message)
				terminal := watermarkPrecisionRead(t, db, id)
				if terminal.LastStatus != status {
					t.Errorf("same-round terminal=%s, want %s; raw=%s stored=%s",
						terminal.LastStatus, status, runAt.Format(time.RFC3339Nano),
						terminal.LastRunAt.Format(time.RFC3339Nano))
				}
				failureLostSuccess := terminal.LastSuccessAt == nil ||
					!terminal.LastSuccessAt.Equal(previous)
				successNotAdvanced := terminal.LastSuccessAt == nil ||
					!terminal.LastSuccessAt.After(previous)
				if status == statusFailed && failureLostSuccess {
					t.Errorf("failure changed prior success: %v", terminal.LastSuccessAt)
				}
				if status == statusSuccess && successNotAdvanced {
					t.Errorf("success did not advance credential: %v", terminal.LastSuccessAt)
				}
				if status == statusFailed &&
					(terminal.LastError == nil || *terminal.LastError != "synthetic source failure") {
					t.Errorf("failure not persisted: %v", terminal.LastError)
				}
			})
		}
	}
}

// TestWatermarkPrecisionMySQLOlderRoundCannotOverwrite 验证旧轮次不能覆盖新轮次。
// 参数：t 为测试上下文，仅连接显式隔离测试库。
// 返回值：无；旧轮次覆盖新状态、轮次时间或成功凭据时失败。
func TestWatermarkPrecisionMySQLOlderRoundCannotOverwrite(t *testing.T) {
	db := watermarkPrecisionMySQL(t)
	previous := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	code := "precision-newer-round"
	r, id := watermarkPrecisionSource(t, db, code, &previous)
	newer := time.Date(2026, 11, 11, 12, 34, 57, 100100000, time.UTC)
	older := time.Date(2026, 11, 11, 12, 34, 56, 999600000, time.UTC)
	r.mark(context.Background(), code, statusRunning, newer, nil)
	before := watermarkPrecisionRead(t, db, id)
	message := "stale failure"
	r.mark(context.Background(), code, statusFailed, older, &message)
	r.mark(context.Background(), code, statusSuccess, older, nil)
	after := watermarkPrecisionRead(t, db, id)
	if after.LastStatus != statusRunning || after.LastRunAt == nil ||
		!after.LastRunAt.Equal(*before.LastRunAt) || after.LastError != nil {
		t.Fatalf("older round overwrote current round: %+v", after)
	}
	if after.LastSuccessAt == nil || !after.LastSuccessAt.Equal(previous) {
		t.Fatalf("older round changed success credential: %v", after.LastSuccessAt)
	}
}
