package metricfacts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// streamFixture 建立只属于当前测试的真实内存 SQL 存储。
// 参数：t 为测试上下文。
// 返回值：具备新旧快照及分块代次表的独立连接。
func streamFixture(t *testing.T) *gorm.DB {
	t.Helper()
	db := metricStoreFixture(t)
	db.Logger = logger.Default.LogMode(logger.Silent)
	if err := db.AutoMigrate(&GenerationRow{}, &FactRow{}); err != nil {
		t.Fatal(err)
	}
	return db
}

// streamTestBody 生成带正确原始字节摘要的合成来源，不调用被测生产实现。
// 参数：metadata 为来源信息，rows 为已知预期事实，chunkSize 为帧行数。
// 返回值：规范 begin/chunk/end 字节序列。
func streamTestBody(metadata Bundle, rows []Row, chunkSize int) []byte {
	var out bytes.Buffer
	metadata.Rows = []Row{}
	encoder := json.NewEncoder(&out)
	_ = encoder.Encode(map[string]any{"type": "begin", "metadata": metadata})
	hash := sha256.New()
	sequence := 1
	for offset := 0; offset < len(rows); offset += chunkSize {
		end := offset + chunkSize
		if end > len(rows) {
			end = len(rows)
		}
		raw := make([]json.RawMessage, 0, end-offset)
		for _, row := range rows[offset:end] {
			value, _ := json.Marshal(row)
			raw = append(raw, value)
			_, _ = hash.Write(value)
			_, _ = hash.Write([]byte{'\n'})
		}
		_ = encoder.Encode(map[string]any{"type": "chunk", "sequence": sequence, "rows": raw})
		sequence++
	}
	_ = encoder.Encode(map[string]any{"type": "end", "row_count": len(rows), "sha256": hex.EncodeToString(hash.Sum(nil)), "source_updated_at": metadata.SourceUpdatedAt, "collection_started_at": metadata.CollectionStartedAt})
	return out.Bytes()
}

// stageTestStream 执行真实分块事务并保留发布步骤供测试单独控制。
// 参数：db 为隔离存储，metadata/body 为可信预期与实际流，now 为时钟。
// 返回值：候选代次或真实校验/存储错误。
func stageTestStream(db *gorm.DB, metadata Bundle, body []byte, now time.Time) (*Staged, error) {
	return StageStream(bytes.NewReader(body), metadata.TenantID, metadata.SourceCode, now,
		func(action func(*gorm.DB) error) error { return db.Transaction(action) })
}

// TestStreamBeyondLegacyLimits 验证超过万条及 8MiB 的合法来源可完整发布，旧协议仍有界。
// 参数：t 为测试上下文，只用独立内存 SQL。
// 返回值：无；事实丢失、暂存提前可见或旧协议被放宽时失败。
func TestStreamBeyondLegacyLimits(t *testing.T) {
	db := streamFixture(t)
	now := time.Now().UTC()
	metadata := Bundle{SchemaVersion: 1, TenantID: "stream-limit", SourceCode: "settlement", SnapshotAt: now.Format(time.RFC3339Nano), Complete: true, Basis: "financial reviewed records", Rows: []Row{}}
	if err := db.Transaction(func(tx *gorm.DB) error { return Save(tx, metadata, now) }); err != nil {
		t.Fatal(err)
	}
	rows := make([]Row, 10001)
	for i := range rows {
		rows[i] = Row{Kind: "reimbursement_unknown", RecordID: fmt.Sprintf("unknown-%05d", i), Period: "2026-11", Status: "UNKNOWN", UnknownReason: strings.Repeat("x", 900), OrganizationID: "org-a", IdentityID: "u1"}
	}
	body := streamTestBody(metadata, rows, 128)
	if len(body) <= MaxBytes {
		t.Fatal("fixture does not exceed original byte limit")
	}
	staged, err := stageTestStream(db, metadata, body, now)
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{DB: db, Now: func() time.Time { return now }}
	before, err := store.Read(context.Background(), metadata.TenantID, "finance", "2026-11", Scope{Mode: "TENANT"})
	if err != nil || before.Unknown != 0 {
		t.Fatalf("unpublished rows visible: %+v %v", before, err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return Publish(tx, staged, now) }); err != nil {
		t.Fatal(err)
	}
	after, err := store.Read(context.Background(), metadata.TenantID, "finance", "2026-11", Scope{Mode: "ORG", OrganizationIDs: []string{"org-a"}})
	if err != nil || after.Unknown != 10001 {
		t.Fatalf("incomplete published source: %+v %v", after, err)
	}
	metadata.Rows = rows
	envelope, _ := json.Marshal(map[string]any{"code": "OK", "data": metadata})
	if _, err := Decode(bytes.NewReader(envelope), metadata.TenantID, metadata.SourceCode, now); err == nil {
		t.Fatal("legacy protocol lost its bounds")
	}
}

// TestStreamFailureKeepsPublishedGeneration 验证损坏、跨块重复和不完整终帧整体拒绝。
// 参数：t 为测试上下文，流仅含合成最小合同事实。
// 返回值：无；任一失败改变旧发布或发布水位时失败。
func TestStreamFailureKeepsPublishedGeneration(t *testing.T) {
	now := time.Now().UTC()
	metadata := Bundle{SchemaVersion: 1, TenantID: "stream-failure", SourceCode: "contract_management", SnapshotAt: now.Format(time.RFC3339Nano), Complete: true, Basis: "frozen price facts", Rows: []Row{}}
	amount, standard := int64(80), int64(100)
	row := Row{Kind: "discount", RecordID: "c1", Period: "2026-11", Status: "KNOWN", Currency: "CNY", AmountMinor: &amount, StandardMinor: &standard, Bucket: "FROM_80_TO_90"}
	good := streamTestBody(metadata, []Row{row}, 1)
	tests := map[string][]byte{
		"truncated":               good[:len(good)-4],
		"trailing":                append(append([]byte{}, good...), []byte("{}\n")...),
		"count":                   bytes.Replace(good, []byte(`"row_count":1`), []byte(`"row_count":2`), 1),
		"digest":                  bytes.Replace(good, []byte(`"sha256":"`), []byte(`"sha256":"0`), 1),
		"sequence":                bytes.Replace(good, []byte(`"sequence":1`), []byte(`"sequence":2`), 1),
		"tenant":                  bytes.Replace(good, []byte(`"tenant_id":"stream-failure"`), []byte(`"tenant_id":"other"`), 1),
		"unknown-field":           bytes.Replace(good, []byte(`"type":"end"`), []byte(`"unknown":1,"type":"end"`), 1),
		"duplicate-across-chunks": streamTestBody(metadata, []Row{row, row}, 1),
		"chunk-limit":             streamTestBody(metadata, makeRepeatedStreamRows(row, 257), 257),
		"blank":                   append([]byte("\n"), good...),
		"oversize-frame":          append([]byte(strings.Repeat(" ", MaxFrameBytes+1)), good...),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			db := streamFixture(t)
			staged, err := stageTestStream(db, metadata, good, now)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Transaction(func(tx *gorm.DB) error { return Publish(tx, staged, now) }); err != nil {
				t.Fatal(err)
			}
			var before SnapshotRow
			db.Take(&before)
			if _, err := stageTestStream(db, metadata, body, now); err == nil {
				t.Fatal("invalid stream accepted")
			}
			var after SnapshotRow
			db.Take(&after)
			if before.GenerationID != after.GenerationID || before.PayloadJSON != after.PayloadJSON {
				t.Fatal("failed stream replaced current generation")
			}
		})
	}
}

// makeRepeatedStreamRows 创建身份不同的同类事实，避免把块数错误混成重复错误。
// 参数：row 为模板，count 为生成数量。
// 返回值：独立记录标识的事实集合。
func makeRepeatedStreamRows(row Row, count int) []Row {
	rows := make([]Row, count)
	for i := range rows {
		rows[i] = row
		rows[i].RecordID = fmt.Sprintf("row-%d", i)
	}
	return rows
}

// TestStreamPublicationRollbackAndRetirement 验证发布回滚及清理不会移除当前完整版本。
// 参数：t 为独立 SQL 测试上下文。
// 返回值：无；回滚指针漂移、跨租户清理或当前事实消失时失败。
func TestStreamPublicationRollbackAndRetirement(t *testing.T) {
	db := streamFixture(t)
	now := time.Now().UTC()
	metadata := Bundle{SchemaVersion: 1, TenantID: "retire", SourceCode: "project_management", SnapshotAt: now.Format(time.RFC3339Nano), Complete: true, Basis: "first review evidence", Rows: []Row{}}
	row := Row{Kind: "report_review", RecordID: "r1", Status: "PENDING", OrganizationID: "org-a", IdentityID: "u1"}
	one, err := stageTestStream(db, metadata, streamTestBody(metadata, []Row{row}, 1), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return Publish(tx, one, now) }); err != nil {
		t.Fatal(err)
	}
	two, err := stageTestStream(db, metadata, streamTestBody(metadata, []Row{}, 1), now)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := Publish(tx, two, now); err != nil {
			return err
		}
		return errors.New("watermark failure")
	})
	if err == nil {
		t.Fatal("publication injection did not fail")
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return cleanupExpiredGeneration(tx, "other-tenant", now.Add(48*time.Hour)) }); err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return cleanupExpiredGeneration(tx, metadata.TenantID, now.Add(48*time.Hour)) }); err != nil {
		t.Fatal(err)
	}
	var count int64
	db.Model(&FactRow{}).Where("generation_id=?", one.id).Count(&count)
	if count != 1 {
		t.Fatal("cleanup removed current generation")
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return Publish(tx, two, now) }); err == nil {
		t.Fatal("expired candidate republished")
	}
}
