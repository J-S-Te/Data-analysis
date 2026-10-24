// Package domain 包含数据分析看板模块拥有的
// 业务值，有意不依赖 HTTP、GORM 或任何
// 其他子系统，使模块可提取而无需携带适配器。
package domain

// 同步水位状态：来自 sync_source.last_status；SNAPSHOT 表示全租户接口快照，
// UNKNOWN 表示没有可核验的同步水位（不能当作“刚刚更新”）。
const (
	SyncStatusSuccess  = "SUCCESS"
	SyncStatusFailed   = "FAILED"
	SyncStatusRunning  = "RUNNING"
	SyncStatusPending  = "PENDING"
	SyncStatusSnapshot = "SNAPSHOT"
	SyncStatusUnknown  = "UNKNOWN"
)

// DataFreshness 区分三个独立时间口径：
//   - SourceUpdatedAt：聚合所依据的源业务数据最后更新时间；
//   - SyncedAt：最近一次成功同步（或持久快照写入）时间；
//   - ComputedAt：本次查询计算时间，不是数据更新时间。
//
// Stale 为 true 时必须同时给出 StaleReason，消费方应展示旧数据并标记陈旧，
// 不得用查询时间掩盖同步停更。
type DataFreshness struct {
	SourceUpdatedAt string `json:"source_updated_at"`
	SyncedAt        string `json:"synced_at"`
	ComputedAt      string `json:"computed_at"`
	SyncStatus      string `json:"sync_status"`
	Stale           bool   `json:"stale"`
	StaleReason     string `json:"stale_reason"`
}

// ContractCurrencyTotal 是单币种合同聚合。不同
// 币种的金额绝不能相加。
type ContractCurrencyTotal struct {
	Currency    string `json:"currency"`
	AmountMinor int64  `json:"amount_minor"`
	Contracts   int64  `json:"contracts"`
}

// ContractSnapshot 是单个租户最新合同看板聚合。
// Available 表示是否已产生聚合快照。
type ContractSnapshot struct {
	Available                 bool                    `json:"available"`
	TenantID                  string                  `json:"tenant_id"`
	SnapshotAt                string                  `json:"snapshot_at"`
	Freshness                 DataFreshness           `json:"freshness"`
	TotalAmountMinor          int64                   `json:"total_amount_minor"`
	CurrencyTotals            []ContractCurrencyTotal `json:"currency_totals"`
	TotalContracts            int64                   `json:"total_contracts"`
	ApprovalContracts         int64                   `json:"approval_contracts"`
	ActiveContracts           int64                   `json:"active_contracts"`
	ExpiredContracts          int64                   `json:"expired_contracts"`
	OpportunityCount          int64                   `json:"opportunity_count"`
	WonContractCount          int64                   `json:"won_contract_count"`
	ConversionAvailable       bool                    `json:"conversion_available"`
	ConversionBasis           string                  `json:"conversion_basis"`
	DiscountAvailable         bool                    `json:"discount_available"`
	DiscountUnavailableReason string                  `json:"discount_unavailable_reason"`
	DiscountBuckets           map[string]int64        `json:"discount_buckets"`
}

// ProjectSnapshot 是单个租户最新项目看板聚合。
// StatusCounts 始终初始化为非 nil 映射，保证 API 值稳定。
type ProjectSnapshot struct {
	Available        bool           `json:"available"`
	TenantID         string         `json:"tenant_id"`
	SnapshotAt       string         `json:"snapshot_at"`
	Freshness        DataFreshness  `json:"freshness"`
	ProjectCount     int            `json:"project_count"`
	InFlightProjects int            `json:"in_flight_projects"`
	RiskProjects     int            `json:"risk_projects"`
	ServiceItems     int            `json:"service_items"`
	StatusCounts     map[string]int `json:"status_counts"`
}

// ContractDetail 是合同下钻列表的脱敏字段集合。
type ContractDetail struct {
	ContractNumber string `json:"contract_number"`
	Title          string `json:"title"`
	Status         string `json:"status"`
	AmountMinor    int64  `json:"amount_minor"`
	Currency       string `json:"currency"`
	EndDate        string `json:"end_date"`
}

// TrendPoint 表示一个月度趋势点。
type TrendPoint struct {
	Period              string                  `json:"period"`
	ContractAmountMinor int64                   `json:"contract_amount_minor"`
	ContractCount       int64                   `json:"contract_count"`
	ProjectCount        int64                   `json:"project_count"`
	CurrencyTotals      []ContractCurrencyTotal `json:"currency_totals"`
}
