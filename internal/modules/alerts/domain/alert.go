// Package domain contains alert business values owned by the data-analysis
// alert module. It has no transport or persistence dependency.
package domain

// Item describes one tenant-scoped operational alert.
type Item struct {
	ID        string  `json:"id"`
	TenantID  string  `json:"tenant_id"`
	AlertType string  `json:"alert_type"`
	RuleCode  string  `json:"rule_code"`
	Severity  string  `json:"severity"`
	TargetRef string  `json:"target_ref"`
	Title     string  `json:"title"`
	DueDate   *string `json:"due_date"`
	Status    string  `json:"status"`
	ClosedAt  *string `json:"closed_at"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
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
