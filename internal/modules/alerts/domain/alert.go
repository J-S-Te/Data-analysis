// Package domain 包含数据分析告警模块拥有的
// 业务值，不依赖传输或持久化。
package domain

// Item 描述一条租户范围内运维告警。
// ClosedReason 区分"规则自动收敛"（AUTO_WINDOW_EXIT/AUTO_CONDITION_CLEARED）与
// "人工关闭"（MANUAL）；ReopenCount 记录规则自动复开次数；AcknowledgedAt/By
// 保留最近一次人工确认证据，规则复开时不清空。
type Item struct {
	ID             string  `json:"id"`
	TenantID       string  `json:"tenant_id"`
	AlertType      string  `json:"alert_type"`
	RuleCode       string  `json:"rule_code"`
	Severity       string  `json:"severity"`
	TargetRef      string  `json:"target_ref"`
	Title          string  `json:"title"`
	DueDate        *string `json:"due_date"`
	Status         string  `json:"status"`
	ClosedAt       *string `json:"closed_at"`
	ClosedReason   string  `json:"closed_reason"`
	ReopenCount    int64   `json:"reopen_count"`
	AcknowledgedAt *string `json:"acknowledged_at"`
	AcknowledgedBy string  `json:"acknowledged_by"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
}

// Summary 是租户范围内预警的聚合统计，供经营总览展示数量与严重度分布。
type Summary struct {
	Total      int64            `json:"total"`
	Open       int64            `json:"open"`
	Ack        int64            `json:"ack"`
	Closed     int64            `json:"closed"`
	BySeverity map[string]int64 `json:"by_severity"`
	ByType     map[string]int64 `json:"by_type"`
}
