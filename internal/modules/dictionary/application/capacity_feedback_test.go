package application

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dictionary/domain"
)

// TestMetricFeedbackCapacityIncludesRetainedDeletes 用途：防止公开指引暗示删除即可释放既有容量。
// 参数：t 为仅使用内存 SQLite 的指标应用测试上下文。
// 返回值：无；保留删除记录被排除或容量拒绝后仍写入时失败。
func TestMetricFeedbackCapacityIncludesRetainedDeletes(t *testing.T) {
	db := testDB(t)
	rows := make([]metricRow, 500)
	for index := range rows {
		rows[index] = metricRow{TenantID: "tenant", Metric: domain.Metric{
			ID: fmt.Sprintf("existing-%d", index), Code: fmt.Sprintf("existing.%d", index),
		}}
	}
	deletedAt := time.Now().UTC()
	rows[499].DeletedAt = &deletedAt
	if err := db.CreateInBatches(rows, 50).Error; err != nil {
		t.Fatal(err)
	}
	_, err := NewCatalogService(db).Create(context.Background(), "tenant", "actor", domain.Metric{
		Code: "custom.new", Name: "指标", Dashboard: "看板", Definition: "定义",
		Formula: "公式", Source: "来源", Period: "月", Status: "待确认",
	})
	expectCode(t, err, "METRIC_LIMIT_EXCEEDED")
	var count int64
	if err = db.Model(&metricRow{}).Count(&count).Error; err != nil || count != 500 {
		t.Fatalf("容量拒绝仍创建记录: count=%d err=%v", count, err)
	}
}
