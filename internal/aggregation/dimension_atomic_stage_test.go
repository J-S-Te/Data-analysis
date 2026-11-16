package aggregation

import (
	"context"
	"errors"
	"gorm.io/gorm"
	"testing"
	"time"
)

// TestDimensionFailedScanKeepsLiveValues 验证第二批失败前暂存不修改任何现有维表。
// 参数：t 为隔离内存测试上下文。
// 返回值：无；扫描失败修改旧代次或丢掉原行时失败。
func TestDimensionFailedScanKeepsLiveValues(t *testing.T) {
	r, db := reconciliationFixture(t)
	if err := db.Exec("INSERT INTO dim_contract VALUES ('a','original')").Error; err != nil {
		t.Fatal(err)
	}
	failure := errors.New("second batch failed")
	err := r.runDimensionSnapshot(context.Background(), newScanTestDB(t), "contract_management", func(_ *gorm.DB, run string) error {
		if err := r.stageDimensionWrite(context.Background(), run, "dim_contract", dimContractUpsert, "NORMAL", "a", "new", nil, "title", "test", "test", "active", nil, nil, nil, nil, int64(9007199254740991), "CNY", "user", "org", "identity", "project", time.Now().UTC()); err != nil {
			return err
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("failure %v", err)
	}
	var ids []string
	if err := db.Table("dim_contract").Pluck("contract_id", &ids).Error; err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "original" {
		t.Fatalf("live changed %v", ids)
	}
	var staged int64
	if err := db.Model(&dimensionSyncKey{}).Where("parent_key='stage'").Count(&staged).Error; err != nil || staged != 0 {
		t.Fatalf("stage %d %v", staged, err)
	}
}
