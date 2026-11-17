package aggregation

import (
	"reflect"
	"testing"
)

// TestContractSourceFiltersPayloadAndKeys 用途：验证分页载荷与对账主键来自同一有效集合。
// 参数：t 为测试上下文，SQLite 仅验证本地查询行为，不冒充 MySQL 验收。
// 返回值：无；旧结构兼容、删除末项或分批过滤有误时失败。
func TestContractSourceFiltersPayloadAndKeys(t *testing.T) {
	for _, marker := range []bool{false, true} {
		t.Run(map[bool]string{false: "old_schema", true: "record_schema"}[marker], func(t *testing.T) {
			db := newScanTestDB(t)
			ddl := "CREATE TABLE con_contract (id TEXT PRIMARY KEY, val TEXT"
			if marker {
				ddl += ", deleted_at DATETIME"
			}
			if err := db.Exec(ddl + ")").Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Exec("INSERT INTO con_contract(id,val) VALUES ('a','first'),('b','second'),('c','third')").Error; err != nil {
				t.Fatal(err)
			}
			if marker {
				if err := db.Exec("UPDATE con_contract SET deleted_at=CURRENT_TIMESTAMP WHERE id='b'").Error; err != nil {
					t.Fatal(err)
				}
			}
			source, err := activeContractSource(db)
			if err != nil {
				t.Fatal(err)
			}
			payload, keys := []string{}, []string{}
			err = scanSourceBatches(source, "con_contract", "id,val", "id", func(row scanStringRow) string { return row.ID }, 1, func(rows []scanStringRow) error {
				for _, row := range rows {
					payload = append(payload, row.ID)
					keys = append(keys, row.ID)
				}
				return nil
			})
			want := []string{"a", "b", "c"}
			if marker {
				want = []string{"a", "c"}
			}
			if err != nil || !reflect.DeepEqual(payload, want) || !reflect.DeepEqual(keys, want) {
				t.Fatalf("payload=%v keys=%v err=%v", payload, keys, err)
			}
			if marker {
				if err := db.Exec("UPDATE con_contract SET deleted_at=CURRENT_TIMESTAMP").Error; err != nil {
					t.Fatal(err)
				}
				var count int64
				if err := source.Table("con_contract").Count(&count).Error; err != nil || count != 0 {
					t.Fatalf("complete empty count=%d err=%v", count, err)
				}
			}
		})
	}
}

// TestContractSourceSchemaFailureIsNotEmpty 用途：验证结构探测失败不会伪装成完整空集合。
// 参数：t 为缺失来源表的隔离测试上下文。
// 返回值：无；吞掉元数据错误或返回可发布查询时失败。
func TestContractSourceSchemaFailureIsNotEmpty(t *testing.T) {
	db := newScanTestDB(t)
	if source, err := activeContractSource(db); err == nil || source != nil {
		t.Fatalf("source=%v err=%v", source, err)
	}
}
