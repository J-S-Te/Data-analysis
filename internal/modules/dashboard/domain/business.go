package domain

// BusinessSnapshot 仅暴露权威来源字段支持的指标。
// 缺失指标须显式呈现，绝不解释为零或计算成功。
type BusinessSnapshot struct {
	Available      bool           `json:"available"`
	TenantID       string         `json:"tenant_id"`
	SnapshotAt     string         `json:"snapshot_at"`
	Metrics        map[string]any `json:"metrics"`
	MissingMetrics []string       `json:"missing_metrics"`
	Basis          string         `json:"basis"`
}
