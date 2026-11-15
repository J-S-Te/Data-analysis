// Package metricfacts 校验业务源完整指标事实并在可信范围内聚合。
package metricfacts

// Bundle 是来源在同一只读事务内形成的完整只读事实集合。
type Bundle struct {
	SchemaVersion       int    `json:"schema_version"`
	TenantID            string `json:"tenant_id"`
	SourceCode          string `json:"source_code"`
	SnapshotAt          string `json:"snapshot_at"`
	SourceUpdatedAt     string `json:"source_updated_at"`
	CollectionStartedAt string `json:"collection_started_at"`
	Complete            bool   `json:"complete"`
	Basis               string `json:"basis"`
	Rows                []Row  `json:"rows"`
}

// Row 保留统计必须的最小维度，不含原始凭证、银行账号或联系方式。
type Row struct {
	Kind             string `json:"kind"`
	RecordID         string `json:"record_id"`
	SubjectID        string `json:"subject_id,omitempty"`
	OrganizationID   string `json:"organization_id"`
	IdentityID       string `json:"identity_id"`
	ProjectID        string `json:"project_id"`
	OrganizationName string `json:"organization_name,omitempty"`
	IdentityName     string `json:"identity_name,omitempty"`
	ProjectName      string `json:"project_name,omitempty"`
	ResourceID       string `json:"resource_id,omitempty"`
	Period           string `json:"period"`
	Status           string `json:"status"`
	Currency         string `json:"currency,omitempty"`
	AmountMinor      *int64 `json:"amount_minor,omitempty"`
	StandardMinor    *int64 `json:"standard_minor,omitempty"`
	TaxMinor         *int64 `json:"tax_minor,omitempty"`
	Bucket           string `json:"bucket,omitempty"`
	UnknownReason    string `json:"unknown_reason,omitempty"`
	FirstDecidedAt   string `json:"first_decided_at,omitempty"`
	ReasonCode       string `json:"reason_code,omitempty"`
	ActualMinutes    *int64 `json:"actual_minutes,omitempty"`
	CapacityMinutes  *int64 `json:"capacity_minutes,omitempty"`
	PendingCount     *int64 `json:"pending_count,omitempty"`
	UnknownCount     *int64 `json:"unknown_count,omitempty"`
	CostCategory     string `json:"cost_category,omitempty"`
}

// Scope 只能由已验签且已在线复核的主体构造，不接收浏览器声明。
type Scope struct {
	Mode            string
	UserID          string
	OrganizationIDs []string
	TeamIDs         []string
}

// Utilization 表示已确认项目实施分钟与显式容量，零分母为null。
type Utilization struct {
	ActualMinutes   int64    `json:"actual_minutes"`
	CapacityMinutes int64    `json:"capacity_minutes"`
	Rate            *float64 `json:"rate"`
	Pending         int64    `json:"pending"`
	Unknown         int64    `json:"unknown"`
}

// CurrencyTotal 按币种区分确认、税额与现金流，允许当期冲回净额为负。
type CurrencyTotal struct {
	Currency        string `json:"currency"`
	IncomeMinor     int64  `json:"income_minor"`
	CostMinor       int64  `json:"cost_minor"`
	IncomeTaxMinor  int64  `json:"income_tax_minor"`
	CostTaxMinor    int64  `json:"cost_tax_minor"`
	SpendMinor      int64  `json:"spend_minor"`
	PrepaymentMinor int64  `json:"prepayment_minor"`
}

// CostDimension 是当前授权范围内的组织、人员或项目净确认成本。
type CostDimension struct {
	Dimension   string `json:"dimension"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	Currency    string `json:"currency"`
	AmountMinor int64  `json:"amount_minor"`
}

// Summary 仅返回范围过滤后的统计，不向浏览器暴露原始来源行。
type Summary struct {
	Available         bool             `json:"available"`
	Section           string           `json:"section"`
	Period            string           `json:"period"`
	Basis             string           `json:"basis"`
	SourceCode        string           `json:"source_code"`
	SnapshotAt        string           `json:"snapshot_at"`
	SourceUpdatedAt   string           `json:"source_updated_at"`
	CollectionStart   string           `json:"collection_started_at"`
	DiscountKnown     int64            `json:"discount_known"`
	DiscountUnknown   int64            `json:"discount_unknown"`
	DiscountBuckets   map[string]int64 `json:"discount_buckets"`
	FirstReviewed     int64            `json:"first_reviewed"`
	FirstPassed       int64            `json:"first_passed"`
	FirstPassRate     *float64         `json:"first_pass_rate"`
	Pending           int64            `json:"pending"`
	Unknown           int64            `json:"unknown"`
	Conflicts         int64            `json:"conflicts"`
	ReturnedDecisions int64            `json:"returned_decisions"`
	ReturnedReports   int64            `json:"returned_reports"`
	ReturnReasons     map[string]int64 `json:"return_reason_counts"`
	Person            Utilization      `json:"person"`
	Equipment         Utilization      `json:"equipment"`
	Currencies        []CurrencyTotal  `json:"currencies"`
	Costs             []CostDimension  `json:"cost_dimensions"`
	Stale             bool             `json:"stale"`
	StaleReason       string           `json:"stale_reason"`
}
