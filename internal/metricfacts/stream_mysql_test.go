package metricfacts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/unified-identity-auth-platform/data-analysis/internal/migration"
	"github.com/unified-identity-auth-platform/data-analysis/migrations"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// localMetricMySQL 迁移本任务显式隔离库，拒绝任何业务库和非回环地址。
// 参数：t 为测试上下文，路径仅从私有 DSN 文件变量读取。
// 返回值：真实 MySQL 连接；缺配置跳过，不安全目标或迁移失败时失败。
func localMetricMySQL(t *testing.T) *gorm.DB {
	t.Helper()
	path := os.Getenv("ANALYSIS_LOCAL_FIX_MYSQL_DSN_FILE")
	if path == "" {
		t.Skip("explicit local isolated MySQL DSN file required")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm()&0077 != 0 {
		t.Fatal("DSN file must be private")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read private DSN file")
	}
	config, err := driver.ParseDSN(strings.TrimSpace(string(raw)))
	if err != nil || config.DBName != "local_fix_analysis" || config.Net != "tcp" || !strings.HasPrefix(config.Addr, "127.0.0.1:") {
		t.Fatal("isolated MySQL guard rejected target")
	}
	if _, err := migration.Run(context.Background(), config.FormatDSN(), migrations.Files); err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(mysql.Open(config.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("open isolated MySQL")
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(4)
	sqlDB.SetMaxIdleConns(2)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// TestStreamMySQLScopeAndPeriods 验证实际迁移和 MySQL 汇总与旧算法的独立预期相同。
// 参数：t 为测试上下文，仅在显式本地隔离库上执行。
// 返回值：无；范围、月份、首轮覆盖、币种或税额差异时失败。
func TestStreamMySQLScopeAndPeriods(t *testing.T) {
	assertGenerationSummaryEquivalence(t, localMetricMySQL(t))
}

// TestLocalMetricMigrationRetry 验证新增DDL在已部分或全部完成后可安全重入。
// 参数：t 为显式隔离MySQL测试上下文；绝不连接业务数据库。
// 返回值：无；重复执行新增DDL失败或旧迁移摘要发生变化时失败。
func TestLocalMetricMigrationRetry(t *testing.T) {
	db := localMetricMySQL(t)
	for range 2 {
		for _, name := range []string{
			"000041_dimension_staged_payload.sql", "000042_metric_fact_generations.sql",
		} {
			raw, err := migrations.Files.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Exec(string(raw)).Error; err != nil {
				t.Fatalf("retry %s: %v", name, err)
			}
		}
	}
}

// TestStreamSQLScopeAndPeriods 对日常回归使用独立内存 SQL，复用同一业务期望。
// 参数：t 为测试上下文。
// 返回值：无；新旧汇总业务语义不一致时失败。
func TestStreamSQLScopeAndPeriods(t *testing.T) {
	assertGenerationSummaryEquivalence(t, streamFixture(t))
}

// assertGenerationSummaryEquivalence 覆盖四分区、窄范围及当前未知证据的读取语义。
// 参数：t 为测试上下文，db 为已隔离且建表完成的数据库。
// 返回值：无；读取差异及跨租户/大小写组织泄露均导致失败。
func assertGenerationSummaryEquivalence(t *testing.T, db *gorm.DB) {
	t.Helper()
	now := time.Now().UTC()
	tenant := fmt.Sprintf("stream-scope-%d", now.UnixNano())
	datasets := streamBusinessRows(now)
	store := &Store{DB: db, Now: func() time.Time { return now }}
	for section, rows := range datasets {
		metadata := Bundle{SchemaVersion: 1, TenantID: tenant, SourceCode: SourceFor(section), SnapshotAt: now.Format(time.RFC3339Nano), Complete: true, Basis: "scope-period-equivalence", Rows: rows}
		if Validate(metadata, tenant, metadata.SourceCode, now) != nil {
			t.Fatalf("invalid independent fixture: %s", section)
		}
		staged, err := stageTestStream(db, metadata, streamTestBody(metadata, rows, 2), now)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Transaction(func(tx *gorm.DB) error { return Publish(tx, staged, now) }); err != nil {
			t.Fatal(err)
		}
		for _, period := range []string{"", "2026-11", "2026-10"} {
			for _, scope := range []Scope{{Mode: "TENANT"}, {Mode: "SELF", UserID: "u1"}, {Mode: "ORG", OrganizationIDs: []string{"org-a"}}, {Mode: "ORG", OrganizationIDs: []string{"ORG-A"}}, {Mode: "TEAM", TeamIDs: []string{"org-a"}}} {
				expected, err := Summarize(metadata, section, period, scope)
				if err != nil {
					t.Fatal(err)
				}
				actual, err := store.Read(context.Background(), tenant, section, period, scope)
				if err != nil {
					t.Fatalf("read %s %s %s: %v", section, period, scope.Mode, err)
				}
				actual.Stale = false
				actual.StaleReason = ""
				if !reflect.DeepEqual(expected, actual) {
					t.Fatalf("summary %s %s %+v differs\nexpected=%+v\nactual=%+v", section, period, scope, expected, actual)
				}
			}
		}
	}
	missing, err := store.Read(context.Background(), tenant+"other", "finance", "", Scope{Mode: "TENANT"})
	if err != nil || missing.Available {
		t.Fatal("cross-tenant snapshot exposed")
	}
}

// streamBusinessRows 创建冻结金额/首轮决定/分钟数据，期望来自业务定义而非 SQL 查询。
// 参数：now 为共同快照时刻。
// 返回值：四分区独立事实；项目和报告先后发布同来源但分别校验。
func streamBusinessRows(now time.Time) map[string][]Row {
	ptr := func(n int64) *int64 { return &n }
	base := Row{OrganizationID: "org-a", IdentityID: "u1", ProjectID: "p1", OrganizationName: "组织甲", IdentityName: "张三", ProjectName: "项目甲", Period: "2026-11", Currency: "CNY"}
	cost := base
	cost.Kind = "recognized_cost"
	cost.RecordID = "cost"
	cost.Status = "CONFIRMED"
	cost.AmountMinor = ptr(100)
	cost.TaxMinor = ptr(6)
	reversal := cost
	reversal.RecordID = "reverse"
	reversal.AmountMinor = ptr(-20)
	reversal.TaxMinor = ptr(-1)
	income := cost
	income.Kind = "recognized_income"
	income.RecordID = "income"
	income.Currency = "USD"
	income.AmountMinor = ptr(50)
	income.TaxMinor = ptr(3)
	spend := cost
	spend.Kind = "reimbursement_spend"
	spend.RecordID = "spend"
	spend.Status = "POSTED"
	spend.AmountMinor = ptr(80)
	spend.TaxMinor = nil
	prepay := spend
	prepay.Kind = "prepayment_spend"
	prepay.RecordID = "prepay"
	prepay.AmountMinor = ptr(5)
	pending := base
	pending.Kind = "recognition_pending"
	pending.RecordID = "pending"
	pending.Status = "PENDING"
	conflict := pending
	conflict.Kind = "recognition_conflict"
	conflict.RecordID = "conflict"
	conflict.Status = "CONFLICT"
	unknown := base
	unknown.Kind = "reimbursement_unknown"
	unknown.RecordID = "unknown"
	unknown.Status = "UNKNOWN"
	unknown.UnknownReason = "missing org evidence"
	hidden := cost
	hidden.RecordID = "hidden"
	hidden.OrganizationID = "org-b"
	hidden.IdentityID = "u2"
	hidden.Period = "2026-10"
	discount := base
	discount.Kind = "discount"
	discount.RecordID = "discount"
	discount.Status = "KNOWN"
	discount.AmountMinor = ptr(80)
	discount.StandardMinor = ptr(100)
	discount.Bucket = "FROM_80_TO_90"
	unknownDiscount := base
	unknownDiscount.Kind = "discount"
	unknownDiscount.RecordID = "old-discount"
	unknownDiscount.Period = ""
	unknownDiscount.Status = "UNKNOWN"
	unknownDiscount.UnknownReason = "no frozen price"
	passed := base
	passed.Kind = "report_review"
	passed.RecordID = "passed"
	passed.Status = "PASSED"
	passed.FirstDecidedAt = "2026-11-01T01:00:00Z"
	returned := passed
	returned.RecordID = "returned"
	returned.Status = "RETURNED"
	undecided := base
	undecided.Kind = "report_review"
	undecided.RecordID = "undecided"
	undecided.Period = ""
	undecided.Status = "UNKNOWN"
	wait := undecided
	wait.RecordID = "wait"
	wait.Status = "PENDING"
	decision := base
	decision.Kind = "report_return"
	decision.RecordID = "decision1"
	decision.Status = "RETURNED"
	decision.SubjectID = "logical-report"
	decision.ReasonCode = "FORMAT"
	second := decision
	second.RecordID = "decision2"
	second.ReasonCode = "EVIDENCE"
	person := base
	person.Kind = "person_utilization"
	person.RecordID = "person"
	person.Status = "CONFIRMED"
	person.ActualMinutes = ptr(40)
	person.AutoConfirmedMinutes = 20
	person.AutoConfirmedCount = 1
	person.CapacityMinutes = ptr(100)
	person.PendingCount = ptr(1)
	person.UnknownCount = ptr(2)
	equipment := person
	equipment.Kind = "equipment_utilization"
	equipment.RecordID = "equipment"
	equipment.ActualMinutes = ptr(60)
	_ = now
	return map[string][]Row{"contract": {discount, unknownDiscount}, "finance": {cost, reversal, income, spend, prepay, pending, conflict, unknown, hidden}, "report": {passed, returned, undecided, wait, decision, second}, "project": {person, equipment}}
}

// TestStreamMySQLRealProducerSamples 交叉消费三个仓库真实生产者导出的合成流。
// 参数：t 为测试上下文，样本目录必须显式提供且不包含真实业务数据。
// 返回值：无；万条事实、金额或完整发布有误时失败。
func TestStreamMySQLRealProducerSamples(t *testing.T) {
	dir := os.Getenv("METRIC_STREAM_SAMPLE_DIR")
	if dir == "" {
		t.Skip("explicit synthetic producer sample directory required")
	}
	db := localMetricMySQL(t)
	for _, name := range []string{"contract", "project", "settlement"} {
		raw, err := os.ReadFile(filepath.Join(dir, name+"-stream.ndjson"))
		if err != nil {
			t.Fatal(err)
		}
		var first struct {
			Metadata Bundle `json:"metadata"`
		}
		if json.Unmarshal(bytes.SplitN(raw, []byte{'\n'}, 2)[0], &first) != nil {
			t.Fatal("bad synthetic sample metadata")
		}
		now := time.Now().UTC()
		staged, err := stageTestStream(db, first.Metadata, raw, now)
		if err != nil {
			t.Fatalf("%s sample rejected: %v", name, err)
		}
		if err := db.Transaction(func(tx *gorm.DB) error { return Publish(tx, staged, now) }); err != nil {
			t.Fatal(err)
		}
		var count int64
		if err := db.Model(&FactRow{}).Where("generation_id=?", staged.id).Count(&count).Error; err != nil || count != 10001 {
			t.Fatalf("%s incomplete: %d %v", name, count, err)
		}
		if name == "settlement" {
			out, err := (&Store{DB: db}).Read(context.Background(), first.Metadata.TenantID, "finance", "", Scope{Mode: "TENANT"})
			if err != nil || len(out.Currencies) != 1 || out.Currencies[0].IncomeMinor != 50005000 || out.Currencies[0].CostMinor != 0 {
				t.Fatalf("financial reconciliation: %+v %v", out, err)
			}
		}
	}
}
