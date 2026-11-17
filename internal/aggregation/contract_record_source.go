package aggregation

import "gorm.io/gorm"

// activeContractSource 用途：兼容旧合同结构，同时从新结构中排除逻辑删除记录。
// 参数：src 为本轮只读事务连接，结构探测和数据读取均沿用此连接。
// 返回值：有效来源查询；结构探测失败必须终止本轮，不能解释成完整空集合。
func activeContractSource(src *gorm.DB) (*gorm.DB, error) {
	columns, err := src.Migrator().ColumnTypes("con_contract")
	if err != nil {
		return nil, err
	}
	for _, column := range columns {
		if column.Name() == "deleted_at" {
			return src.Where("deleted_at IS NULL").Session(&gorm.Session{}), nil
		}
	}
	return src.Session(&gorm.Session{}), nil
}
