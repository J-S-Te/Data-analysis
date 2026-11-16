package aggregation

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/unified-identity-auth-platform/data-analysis/internal/metricfacts"
	"github.com/unified-identity-auth-platform/data-analysis/internal/migration"
	"github.com/unified-identity-auth-platform/data-analysis/migrations"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// metricStreamLocalDB 打开本轮唯一允许的 MySQL schema 并应用真实迁移。
// 参数：t 为测试上下文；凭据从显式私有文件读取且不输出。
// 返回值：隔离 MySQL；未提供文件时跳过，不安全目标立即失败。
func metricStreamLocalDB(t *testing.T) *gorm.DB {
	t.Helper()
	file := os.Getenv("ANALYSIS_LOCAL_FIX_MYSQL_DSN_FILE")
	if file == "" {
		t.Skip("explicit isolated MySQL DSN file required")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal("read isolated DSN")
	}
	cfg, err := driver.ParseDSN(strings.TrimSpace(string(raw)))
	if err != nil || cfg.DBName != "local_fix_analysis" || cfg.Net != "tcp" || !strings.HasPrefix(cfg.Addr, "127.0.0.1:") {
		t.Fatal("isolated MySQL target guard rejected")
	}
	if _, err := migration.Run(context.Background(), cfg.FormatDSN(), migrations.Files); err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(mysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("open isolated MySQL")
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// TestMetricStreamMySQLPublicationAndWatermark 验证完整结束与 SUCCESS 必须在同一真实事务提交。
// 参数：t 为隔离 MySQL 测试上下文，HTTP 来源只使用合成数据。
// 返回值：无；损坏帧或成功水位失败覆盖旧代次、旧成功时间时失败。
func TestMetricStreamMySQLPublicationAndWatermark(t *testing.T) {
	db := metricStreamLocalDB(t)
	tenant := fmt.Sprintf("strm%019d", time.Now().UnixNano())
	basis := "first complete generation"
	stamp := time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)
	corrupt := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/stream") {
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = fmt.Fprint(w, emptyMetricStream(tenant, basis, stamp))
			if corrupt {
				_, _ = fmt.Fprint(w, "{}\n")
			}
			return
		}
		_, _ = fmt.Fprintf(w, `{"code":"OK","data":{"tenant_id":%q,"total_amount_minor":0,"total_contracts":0,"currency_totals":[],"approval_contracts":0,"active_contracts":0,"expired_contracts":0}}`, tenant)
	}))
	defer server.Close()
	api := NewAPISyncRunner(db, APISyncOptions{TenantID: tenant, MetricFactsEnabled: true, ContractInternalURL: server.URL + "/internal/v1/dashboard", ContractCredential: MachineCredential{ClientID: "atomic-stream", Scope: "dashboard.contract.read"}})
	api.tokens["atomic-stream"] = cachedMachineToken{value: "synthetic-token", expiresAt: time.Now().Add(time.Hour)}
	runner := &Runner{aggDB: db, tenantID: tenant, ProjectAPI: api}
	if err := runner.EnsureSyncSources(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, watermark := metricAtomicState(t, runner)
	if before.GenerationID == "" {
		t.Fatal("stream not published as a generation")
	}
	basis = "second complete generation"
	stamp = time.Now().UTC().Format(time.RFC3339Nano)
	corrupt = true
	if err := runner.RunOnce(context.Background()); err == nil {
		t.Fatal("trailing stream accepted")
	}
	assertMetricStreamUnchanged(t, runner, before, watermark)
	corrupt = false
	callback := "stream-watermark-failure"
	if err := db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		updates, ok := tx.Statement.Dest.(map[string]any)
		if ok && tx.Statement.Table == "sync_source" && updates["last_status"] == "SUCCESS" && strings.Contains(fmt.Sprint(tx.Statement.Clauses["WHERE"]), "contract_metrics") {
			tx.AddError(errors.New("synthetic watermark failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := runner.RunOnce(context.Background()); err == nil {
		t.Fatal("watermark failure concealed")
	}
	if err := db.Callback().Update().Remove(callback); err != nil {
		t.Fatal(err)
	}
	assertMetricStreamUnchanged(t, runner, before, watermark)
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, success := metricAtomicState(t, runner)
	if after.GenerationID == before.GenerationID || success.LastStatus != "SUCCESS" {
		t.Fatal("retry did not publish complete replacement")
	}
}

// assertMetricStreamUnchanged 检查失败采集保留已发布代次及最后成功时间。
// 参数：t/runner 为上下文，before/watermark 为故障前独立读取的持久化状态。
// 返回值：无；任一成功事实被错误推进时失败。
func assertMetricStreamUnchanged(t *testing.T, runner *Runner, before metricfacts.SnapshotRow, watermark SyncSource) {
	t.Helper()
	after, state := metricAtomicState(t, runner)
	if after.GenerationID != before.GenerationID || after.PayloadJSON != before.PayloadJSON || watermark.LastSuccessAt == nil || state.LastSuccessAt == nil || !watermark.LastSuccessAt.Equal(*state.LastSuccessAt) {
		t.Fatal("failed collection changed successful publication")
	}
}
