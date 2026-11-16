package metricfacts

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"gorm.io/gorm"
)

// TestStreamRollbackFailsClosed 验证旧读协议不会把非零流式事实误报为可用零值。
// 参数：t 为隔离SQL上下文，事实来自合成完整流。
// 返回值：无；旧Validate接受元数据空行或新读者漏记真实事实时失败。
func TestStreamRollbackFailsClosed(t *testing.T) {
	db := streamFixture(t)
	now := time.Now().UTC()
	for _, rows := range [][]Row{
		{{Kind: "discount", RecordID: "one", Status: "UNKNOWN", UnknownReason: "合成未定价"}},
		{},
	} {
		metadata := Bundle{SchemaVersion: 1, TenantID: "rollback-test", SourceCode: "contract_management",
			SnapshotAt: now.Format(time.RFC3339Nano), Complete: true, Basis: "合成完整流", Rows: []Row{}}
		staged, err := stageTestStream(db, metadata, streamTestBody(metadata, rows, 1), now)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Transaction(func(tx *gorm.DB) error { return Publish(tx, staged, now) }); err != nil {
			t.Fatal(err)
		}
		var persisted SnapshotRow
		if err := db.Take(&persisted).Error; err != nil {
			t.Fatal(err)
		}
		// 旧Store只读取payload_json并执行同一v1 Validate，不认识generation_id。
		var legacy Bundle
		if json.Unmarshal([]byte(persisted.PayloadJSON), &legacy) == nil &&
			Validate(legacy, metadata.TenantID, metadata.SourceCode, now) == nil {
			t.Fatal("old reader would report the generation as a valid empty bundle")
		}
		store := Store{DB: db, Now: func() time.Time { return now }}
		actual, err := store.Read(context.Background(), metadata.TenantID, "contract", "", Scope{Mode: "TENANT"})
		if err != nil || !actual.Available || actual.DiscountUnknown != int64(len(rows)) {
			t.Fatalf("current reader lost generation: %+v %v", actual, err)
		}
	}
}
