// Package domain contains the business values owned by the data-analysis
// dashboard module. It deliberately has no dependency on HTTP, GORM or any
// other subsystem, so the module can be extracted without carrying adapters.
package domain

// ContractSnapshot is the latest contract dashboard aggregate for one tenant.
// Available reports whether an aggregation snapshot has been produced yet.
type ContractSnapshot struct {
	Available         bool
	TenantID          string
	SnapshotAt        string
	TotalAmountMinor  int64
	TotalContracts    int64
	ApprovalContracts int64
	ActiveContracts   int64
	ExpiredContracts  int64
}

// ProjectSnapshot is the latest project dashboard aggregate for one tenant.
// StatusCounts is always initialized to a non-nil map for a stable API value.
type ProjectSnapshot struct {
	Available        bool
	TenantID         string
	SnapshotAt       string
	ProjectCount     int
	InFlightProjects int
	RiskProjects     int
	ServiceItems     int
	StatusCounts     map[string]int
}
