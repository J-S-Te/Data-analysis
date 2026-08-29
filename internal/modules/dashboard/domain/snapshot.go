// Package domain contains the business values owned by the data-analysis
// dashboard module. It deliberately has no dependency on HTTP, GORM or any
// other subsystem, so the module can be extracted without carrying adapters.
package domain

// ContractSnapshot is the latest contract dashboard aggregate for one tenant.
// Available reports whether an aggregation snapshot has been produced yet.
type ContractSnapshot struct {
	Available         bool             `json:"available"`
	TenantID          string           `json:"tenant_id"`
	SnapshotAt        string           `json:"snapshot_at"`
	TotalAmountMinor  int64            `json:"total_amount_minor"`
	TotalContracts    int64            `json:"total_contracts"`
	ApprovalContracts int64            `json:"approval_contracts"`
	ActiveContracts   int64            `json:"active_contracts"`
	ExpiredContracts  int64            `json:"expired_contracts"`
	OpportunityCount  int64            `json:"opportunity_count"`
	WonContractCount  int64            `json:"won_contract_count"`
	DiscountBuckets   map[string]int64 `json:"discount_buckets"`
}

// ProjectSnapshot is the latest project dashboard aggregate for one tenant.
// StatusCounts is always initialized to a non-nil map for a stable API value.
type ProjectSnapshot struct {
	Available        bool           `json:"available"`
	TenantID         string         `json:"tenant_id"`
	SnapshotAt       string         `json:"snapshot_at"`
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
	EndDate        string `json:"end_date"`
}

// TrendPoint 表示一个月度趋势点。
type TrendPoint struct {
	Period              string `json:"period"`
	ContractAmountMinor int64  `json:"contract_amount_minor"`
	ContractCount       int64  `json:"contract_count"`
	ProjectCount        int64  `json:"project_count"`
}
