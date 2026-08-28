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
	UpdatedBy     *string   `json:"updated_by"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}
