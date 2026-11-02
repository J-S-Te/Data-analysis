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

// appendTestDB 构造带 outbox 表的内存审计库（表结构由 outboxRow 推导）。
// 参数：t 为测试上下文。
// 返回值：已建表的 GORM 句柄。
func appendTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "audit-append.sqlite")),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
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
	return db
}

// storedPayload 读回一行的 request_id/payload 原值（nil 表示 SQL NULL）。
type storedPayload struct {
	RequestID *string `gorm:"column:request_id"`
	Payload   *string `gorm:"column:payload"`
}

func readStored(t *testing.T, db *gorm.DB, id string) storedPayload {
	t.Helper()
	var row storedPayload
	if err := db.Table("bi_audit_outbox").Select("request_id, payload").Where("id = ?", id).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row
}

func outboxMaxID(t *testing.T, db *gorm.DB) string {
	t.Helper()
	var id string
	if err := db.Table("bi_audit_outbox").Select("id").Order("id DESC").Limit(1).Scan(&id).Error; err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("append 没有写入任何 outbox 行")
	}
	return id
}

// TestOutboxRowDeclaresRequestIDAndPayloadColumns 固定隐患面：
// outboxRow 声明了 request_id/payload，读取侧 Claim 用 SELECT *，缺列时不会报错而是
// 把这两列静默读成零值（追踪号与本地证据丢失）；写入侧按列名 INSERT，缺列则是
// ERROR 1054 Unknown column，整条业务事务回滚。所以结构体与迁移列必须一一对应。
func TestOutboxRowDeclaresRequestIDAndPayloadColumns(t *testing.T) {
	t.Parallel()
	db := appendTestDB(t)
	statement := &gorm.Statement{DB: db}
	if err := statement.Parse(&outboxRow{}); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"request_id", "payload"} {
		if statement.Schema.LookUpField(column) == nil {
			t.Fatalf("outboxRow 缺少 %s 列，Claim 的 SELECT 会带上不存在的列", column)
		}
		if !db.Migrator().HasColumn("bi_audit_outbox", column) {
			t.Fatalf("bi_audit_outbox 缺少 %s 列（迁移 000028 必须补上）", column)
		}
	}
	// 结构体声明与迁移列名必须一致，避免两侧各自漂移。
	if got := statement.Schema.LookUpField("request_id").DBName; got != "request_id" {
		t.Fatalf("request_id 的 DBName = %q", got)
	}
	if got := statement.Schema.LookUpField("payload").DBName; got != "payload" {
		t.Fatalf("payload 的 DBName = %q", got)
	}
}

// TestAppendWritesRequestIDAndValidPayload 验证迁移 000028 的两列真的被写入，
// 并且 Claim 能把它们读回 Entry（投递器据此保留请求追踪号）。
func TestAppendWritesRequestIDAndValidPayload(t *testing.T) {
	db := appendTestDB(t)
	if err := Append(db, Entry{
		TenantID: "tenant-1", Action: "DATA_ANALYSIS:REPORT_STATUS_READ",
		ResourceType: "contract_import_item", ResourceID: "item-1",
		RequestID: "req-42", Payload: `{"source_revision":2,"report_release_version":3,"state":"APPLIED"}`,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	id := outboxMaxID(t, db)
	stored := readStored(t, db, id)
	if stored.RequestID == nil || *stored.RequestID != "req-42" {
		t.Fatalf("request_id = %v，期望 req-42", stored.RequestID)
	}
	if stored.Payload == nil || *stored.Payload != `{"source_revision":2,"report_release_version":3,"state":"APPLIED"}` {
		t.Fatalf("payload = %v", stored.Payload)
	}

	store := &GORMStore{DB: db, Lease: 2 * time.Minute}
	entries, err := store.Claim(context.Background(), "worker", 10, time.Now().UTC())
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("Claim 返回 %d 条，期望 1 条", len(entries))
	}
	if entries[0].RequestID != "req-42" {
		t.Fatalf("Entry.RequestID = %q", entries[0].RequestID)
	}
	if entries[0].Payload == "" {
		t.Fatal("Entry.Payload 为空：payload 没有从列读回")
	}
}

// TestAppendStoresNullForEmptyOrInvalidPayload 验证空/非法负载一律落 NULL：
// 空串或非法 JSON 若写进 MySQL JSON 列，会让整条业务事务（业务变更 + 审计）一起失败。
func TestAppendStoresNullForEmptyOrInvalidPayload(t *testing.T) {
	cases := []struct {
		name    string
		request string
		payload string
	}{
		{name: "空负载", request: "req-null", payload: ""},
		{name: "仅空白", request: "req-blank", payload: "   "},
		{name: "非法 JSON", request: "req-bad", payload: "{not json"},
		{name: "裸字符串非法", request: "req-text", payload: "source_revision=2"},
		{name: "空 request_id 与空负载", request: "", payload: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := appendTestDB(t)
			if err := Append(db, Entry{
				TenantID: "tenant-1", Action: "DATA_ANALYSIS:ALERT_ACK",
				ResourceType: "alert", ResourceID: "alert-1",
				RequestID: tc.request, Payload: tc.payload,
			}); err != nil {
				t.Fatalf("Append: %v", err)
			}
			id := outboxMaxID(t, db)
			stored := readStored(t, db, id)
			if stored.Payload != nil {
				t.Fatalf("空/非法 payload 必须落 NULL，实际 %q", *stored.Payload)
			}
			if tc.request == "" {
				if stored.RequestID != nil {
					t.Fatalf("空 request_id 必须落 NULL，实际 %q", *stored.RequestID)
				}
				return
			}
			if stored.RequestID == nil || *stored.RequestID != tc.request {
				t.Fatalf("request_id = %v，期望 %q", stored.RequestID, tc.request)
			}
		})
	}
}

// TestAppendOmitsRequestColumnsWhenEmpty 固定滚动发布窗口的降级行为：
// 没有 request_id/payload 时不写这两列，旧库（000028 未执行）也不会因为缺列
// 让整条业务事务失败。
func TestAppendOmitsRequestColumnsWhenEmpty(t *testing.T) {
	columns := map[string]any{}
	if value := nullable(""); value != nil {
		columns["request_id"] = value
	}
	if value := payloadJSON(""); value != nil {
		columns["payload"] = value
	}
	if len(columns) != 0 {
		t.Fatalf("空值不应写入 request_id/payload：%v", columns)
	}
	if payloadJSON(`{"a":1}`) == nil {
		t.Fatal("合法 JSON 必须写入 payload")
	}
}
