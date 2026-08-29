// Package domain contains the business values owned by the data-analysis
// dashboard module. It deliberately has no dependency on HTTP, GORM or any
// other subsystem, so the module can be extracted without carrying adapters.
package domain

// ContractSnapshot is the latest contract dashboard aggregate for one tenant.
// Available reports whether an aggregation snapshot has been produced yet.
type ContractSnapshot struct {
	Available         bool   `json:"available"`
	TenantID          string `json:"tenant_id"`
	SnapshotAt        string `json:"snapshot_at"`
	TotalAmountMinor  int64  `json:"total_amount_minor"`
	TotalContracts    int64  `json:"total_contracts"`
	ApprovalContracts int64  `json:"approval_contracts"`
	ActiveContracts   int64  `json:"active_contracts"`
	ExpiredContracts  int64  `json:"expired_contracts"`
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
