package aggregation

import (
	"context"

	"gorm.io/gorm"
)

// writeAPISnapshot 将API快照写入纳入相同的许可、取消和手工任务租约事务。
// 参数：ctx 为本轮执行上下文，statement/args 为调用方固定SQL及已校验快照值。
// 返回值：执行权限或持久化错误；失去租约的迟到请求不能发布快照。
func (r *APISyncRunner) writeAPISnapshot(ctx context.Context, statement string, args ...any) error {
	guard := &Runner{aggDB: r.db, tenantID: r.options.TenantID, LicenseCheck: r.options.LicenseCheck}
	return guard.aggregationWrite(ctx, func(tx *gorm.DB) error {
		return tx.Exec(statement, args...).Error
	})
}
