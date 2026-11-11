package aggregation

// ValidateSnapshotRetentionPolicy 供组合根在连接数据库前校验快照留存配置。
// 参数：policy 为可含零值默认项的策略。
// 返回值：有效且补全默认的策略，或非法范围错误。
func ValidateSnapshotRetentionPolicy(policy SnapshotRetentionPolicy) (SnapshotRetentionPolicy, error) {
	return policy.normalized()
}
