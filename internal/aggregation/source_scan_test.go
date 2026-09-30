package aggregation

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// scanTestRow 是游标分批扫描（AUD-2026-030）测试用的最小行结构（数值主键）。
type scanTestRow struct {
	ID  uint64 `gorm:"column:id;primaryKey"`
	Val string `gorm:"column:val"`
}

// scanStringRow 覆盖字符串业务主键场景（对应 con_contract.id）。
type scanStringRow struct {
	ID  string `gorm:"column:id;primaryKey"`
	Val string `gorm:"column:val"`
}

var scanTestDBSeq atomic.Uint64

// newScanTestDB 打开一个独立的 sqlite 内存库并建好扫描测试表。
func newScanTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:scan_test_%d?mode=memory&cache=shared", scanTestDBSeq.Add(1))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get sql db: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.Exec(`CREATE TABLE scan_rows (id INTEGER PRIMARY KEY, val TEXT)`).Error; err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

func insertScanRows(t *testing.T, db *gorm.DB, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		if err := db.Exec(`INSERT INTO scan_rows (id, val) VALUES (?, ?)`, i, fmt.Sprintf("row-%d", i)).Error; err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}
}

// collectScanRows 用指定批大小扫描 scan_rows，返回全部行与实际批数。
func collectScanRows(db *gorm.DB, batchSize int) ([]scanTestRow, int, error) {
	var collected []scanTestRow
	batches := 0
	err := scanSourceBatches(db, "scan_rows", "id, val", "id", func(row scanTestRow) uint64 { return row.ID }, batchSize, func(rows []scanTestRow) error {
		batches++
		collected = append(collected, rows...)
		return nil
	})
	return collected, batches, err
}

// AUD-2026-030：超过 500 行的源表被拆成多个键集批次读取，结果（行数、顺序、无重复、
// 批间无缝）与单批全量读取完全一致。
func TestScanSourceBatchesSplitsLargeTableAcrossKeysetBatches(t *testing.T) {
	t.Parallel()
	db := newScanTestDB(t)
	const total = 1200 // 500 + 500 + 200
	insertScanRows(t, db, total)

	multi, multiBatches, err := collectScanRows(db, sourceReadBatchSize)
	if err != nil {
		t.Fatalf("scanSourceBatches(multi) error = %v", err)
	}
	if multiBatches != 3 {
		t.Fatalf("batches = %d, want 3 (500+500+200)", multiBatches)
	}
	if len(multi) != total {
		t.Fatalf("collected rows = %d, want %d", len(multi), total)
	}
	seen := make(map[uint64]bool, total)
	for i, row := range multi {
		if row.ID != uint64(i+1) {
			t.Fatalf("row[%d].ID = %d, want %d (keyset order must be ascending and gapless)", i, row.ID, i+1)
		}
		if seen[row.ID] {
			t.Fatalf("duplicate row id %d across batches", row.ID)
		}
		seen[row.ID] = true
		if row.Val != fmt.Sprintf("row-%d", row.ID) {
			t.Fatalf("row %d payload mismatch: %q", row.ID, row.Val)
		}
	}

	single, singleBatches, err := collectScanRows(db, total+1)
	if err != nil {
		t.Fatalf("scanSourceBatches(single) error = %v", err)
	}
	if singleBatches != 1 || len(single) != total {
		t.Fatalf("single-batch scan: batches = %d rows = %d, want 1 batch / %d rows", singleBatches, len(single), total)
	}
	for i := range multi {
		if multi[i] != single[i] {
			t.Fatalf("row %d differs between multi-batch and single-batch scan: %#v vs %#v", i, multi[i], single[i])
		}
	}
}

// AUD-2026-030：行数恰为批大小整数倍时，最后一批读满后继续游标查询必须正常终止，
// 不得丢行也不得死循环。
func TestScanSourceBatchesTerminatesOnExactBatchMultiple(t *testing.T) {
	t.Parallel()
	db := newScanTestDB(t)
	const total = 2 * sourceReadBatchSize
	insertScanRows(t, db, total)

	rows, batches, err := collectScanRows(db, sourceReadBatchSize)
	if err != nil {
		t.Fatalf("scanSourceBatches error = %v", err)
	}
	if batches != 2 {
		t.Fatalf("batches = %d, want 2", batches)
	}
	if len(rows) != total {
		t.Fatalf("collected rows = %d, want %d", len(rows), total)
	}
	if rows[len(rows)-1].ID != total {
		t.Fatalf("last row id = %d, want %d", rows[len(rows)-1].ID, total)
	}
}

// AUD-2026-030：空表零批次返回且不报错（对应 syncContract 的 contract source empty 语义）。
func TestScanSourceBatchesEmptyTableReturnsNoBatches(t *testing.T) {
	t.Parallel()
	db := newScanTestDB(t)

	rows, batches, err := collectScanRows(db, sourceReadBatchSize)
	if err != nil {
		t.Fatalf("scanSourceBatches error = %v", err)
	}
	if batches != 0 || len(rows) != 0 {
		t.Fatalf("batches = %d rows = %d, want 0/0 for empty source", batches, len(rows))
	}
}

// AUD-2026-030：onBatch 错误必须原样透传并终止扫描（禁止吞错），不再触发后续批次。
func TestScanSourceBatchesPropagatesOnBatchError(t *testing.T) {
	t.Parallel()
	db := newScanTestDB(t)
	insertScanRows(t, db, 3*sourceReadBatchSize)

	batchErr := errors.New("upsert failed")
	calls := 0
	err := scanSourceBatches(db, "scan_rows", "id, val", "id", func(row scanTestRow) uint64 { return row.ID }, sourceReadBatchSize, func([]scanTestRow) error {
		calls++
		return batchErr
	})
	if !errors.Is(err, batchErr) {
		t.Fatalf("error = %v, want %v", err, batchErr)
	}
	if calls != 1 {
		t.Fatalf("onBatch calls = %d, want 1 (scan must stop after failure)", calls)
	}
}

// AUD-2026-030：字符串业务主键（con_contract.id 口径）同样支持键集游标分批，
// 顺序与完整性保持一致。
func TestScanSourceBatchesSupportsStringPrimaryKey(t *testing.T) {
	t.Parallel()
	dsn := fmt.Sprintf("file:scan_test_%d?mode=memory&cache=shared", scanTestDBSeq.Add(1))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.Exec(`CREATE TABLE scan_string_rows (id TEXT PRIMARY KEY, val TEXT)`).Error; err != nil {
		t.Fatalf("create table: %v", err)
	}
	const total = 600
	for i := 1; i <= total; i++ {
		// 固定宽度零填充，保证字典序与数值序一致（模拟有序业务主键）。
		key := fmt.Sprintf("CON-%06d", i)
		if err := db.Exec(`INSERT INTO scan_string_rows (id, val) VALUES (?, ?)`, key, key).Error; err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}

	var collected []scanStringRow
	batches := 0
	err = scanSourceBatches(db, "scan_string_rows", "id, val", "id", func(row scanStringRow) string { return row.ID }, sourceReadBatchSize, func(rows []scanStringRow) error {
		batches++
		collected = append(collected, rows...)
		return nil
	})
	if err != nil {
		t.Fatalf("scanSourceBatches error = %v", err)
	}
	if batches != 2 {
		t.Fatalf("batches = %d, want 2", batches)
	}
	if len(collected) != total {
		t.Fatalf("collected rows = %d, want %d", len(collected), total)
	}
	for i, row := range collected {
		if want := fmt.Sprintf("CON-%06d", i+1); row.ID != want {
			t.Fatalf("row[%d].ID = %q, want %q", i, row.ID, want)
		}
	}
}
