package auditoutbox

import (
	"context"
	"errors"
)

// OutboxCounts 是 bi_audit_outbox 的只读状态计数（C2 指标口径）。
//
// 三态映射：
//   - Pending 覆盖 pending / retry / processing：仍在投递链路上，可能因退避或
//     认领租约尚未到期而暂时不动，但事件没有丢；
//   - Delivered 是已成功投递的平台审计事件（保留窗口内累积）；
//   - Dead 是 attempts 达到上限后停在 failed 的事件，只能人工重放，
//     因此必须单独暴露，不能被“pending 很少”掩盖。
type OutboxCounts struct {
	Pending   int64
	Delivered int64
	Dead      int64
}

// Counts 用一次分组查询读取 outbox 三态计数。
// 参数：ctx 为查询上下文（抓取请求的生命周期）。
// 返回值：三态计数；查询失败返回错误，调用方不得把错误当成 0 展示。
// 说明：只读单条聚合查询，不触碰租约与状态，可在 /metrics 抓取时安全重复调用。
func (s *GORMStore) Counts(ctx context.Context) (OutboxCounts, error) {
	if s == nil || s.DB == nil {
		return OutboxCounts{}, errors.New("audit outbox store is not initialized")
	}
	var rows []struct {
		DeliveryStatus string `gorm:"column:delivery_status"`
		Total          int64  `gorm:"column:total"`
	}
	if err := s.DB.WithContext(ctx).Model(&outboxRow{}).
		Select("delivery_status, COUNT(*) AS total").
		Group("delivery_status").
		Scan(&rows).Error; err != nil {
		return OutboxCounts{}, err
	}
	var counts OutboxCounts
	for _, row := range rows {
		switch row.DeliveryStatus {
		case "delivered":
			counts.Delivered += row.Total
		case "failed":
			counts.Dead += row.Total
		default:
			// 未识别的状态按“未投递完成”计入 pending：宁可多报积压，
			// 也不能把一条来路不明的行当成已投递或已死亡。
			counts.Pending += row.Total
		}
	}
	return counts, nil
}
