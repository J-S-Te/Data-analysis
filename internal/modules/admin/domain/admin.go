// Package domain 定义数据看板管理模块拥有的数据源、同步任务和预警规则。
package domain

import "time"

// SyncSource 描述一个可由聚合 Worker 消费的数据源状态。
type SyncSource struct {
	TenantID      string     `json:"-"`
	ID            string     `json:"id"`
	SubsystemCode string     `json:"subsystem_code"`
	DBHost        string     `json:"db_host"`
	DBSchema      string     `json:"db_schema"`
	Enabled       bool       `json:"enabled"`
	LastStatus    string     `json:"last_status"`
	LastRunAt     *time.Time `json:"last_run_at"`
	LastError     *string    `json:"last_error"`
	LastErrorCode string     `json:"last_error_code,omitempty"`
}

// SyncJob 表示已经入队、等待聚合 Worker 执行的一次同步请求。
type SyncJob struct {
	TenantID      string    `json:"-"`
	ID            string    `json:"id"`
	SourceID      string    `json:"source_id"`
	SubsystemCode string    `json:"subsystem_code"`
	Status        string    `json:"status"`
	RequestedAt   time.Time `json:"requested_at"`
}

// AlertRule 定义一个业务租户自己的预警规则。
type AlertRule struct {
	TenantID      string    `json:"-"`
	ID            string    `json:"id"`
	RuleCode      string    `json:"rule_code"`
	Name          string    `json:"name"`
	SourceFCT     string    `json:"source_fct"`
	Severity      string    `json:"severity"`
	Enabled       bool      `json:"enabled"`
	ThresholdJSON *string   `json:"threshold_json"`
	ScopeJSON     *string   `json:"scope_json"`
	EffectiveFrom *string   `json:"effective_from"`
	EffectiveTo   *string   `json:"effective_to"`
	UpdatedBy     *string   `json:"updated_by"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	Version       int64     `json:"version"`
}

// AlertRuleScope 是合同到期规则的可选适用范围；零值表示不限制。
type AlertRuleScope struct {
	OwnerOrgIDs    []string `json:"owner_org_ids"`
	CustomerTypes  []string `json:"customer_types"`
	AmountMinMinor *int64   `json:"amount_min_minor"`
	AmountMaxMinor *int64   `json:"amount_max_minor"`
}

// AlertPreviewItem 是命中预览中的最小合同摘要，不返回客户敏感信息。
type AlertPreviewItem struct {
	ContractID   string `json:"contract_id"`
	Title        string `json:"title"`
	OwnerOrgID   string `json:"owner_org_id"`
	CustomerType string `json:"customer_type"`
	AmountMinor  int64  `json:"amount_minor"`
	DueDate      string `json:"due_date"`
}

// AlertPreviewResult 是规则命中预览结果。
type AlertPreviewResult struct {
	MatchedCount int64              `json:"matched_count"`
	Sample       []AlertPreviewItem `json:"sample"`
	Limited      bool               `json:"limited"`
}
