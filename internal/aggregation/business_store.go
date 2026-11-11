package aggregation

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// businessSourceCode 返回看板分区对应的权威来源编码。
// 参数：section 为报告或联动财务分区。
// 返回值：固定来源码，未知分区为空。
func businessSourceCode(section string) string {
	if section == "report" {
		return "project_management"
	}
	if section == "finance" {
		return "settlement"
	}
	return ""
}

// syncBusinessHTTP 只在完整API快照验证成功后发布本地指标。
// 参数：ctx 为许可/租约上下文，section/endpoint/credential 为受控来源。
// 返回值：来源或持久化错误；任何失败均保留上一成功快照。
func (r *Runner) syncBusinessHTTP(ctx context.Context, section, endpoint string, credential MachineCredential) error {
	if r.ProjectAPI == nil {
		return errors.New("business statistics API runner is unavailable")
	}
	value, err := r.ProjectAPI.readBusinessSnapshot(ctx, endpoint, credential, section)
	if err != nil {
		return err
	}
	return r.storeBusiness(ctx, section, value)
}

// verifyBusinessWatermark 在当前聚合事务内拒绝同来源迟到快照覆盖较新事实。
// 参数：tx 为持有当前租约的事务，tenant/section 为存储键，next 为待发布快照。
// 返回值：缺旧行或水位不倒退时nil；存储故障、旧数据损坏或倒退时错误。
func verifyBusinessWatermark(tx *gorm.DB, tenant, section string, next domain.BusinessSnapshot) error {
	if next.SourceSnapshotAt == "" {
		return nil
	}
	var old businessRow
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("tenant_id = ? AND section = ?", tenant, section).Take(&old).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var current domain.BusinessSnapshot
	if err := json.Unmarshal([]byte(old.Payload), &current); err != nil {
		return errors.New("stored business snapshot is invalid")
	}
	if current.SourceCode != next.SourceCode || current.SourceSnapshotAt == "" {
		return nil
	}
	previous, err := time.Parse(time.RFC3339Nano, current.SourceSnapshotAt)
	if err != nil {
		return errors.New("stored business source watermark is invalid")
	}
	incoming, err := time.Parse(time.RFC3339Nano, next.SourceSnapshotAt)
	if err != nil || incoming.Before(previous) {
		return errors.New("business source watermark regressed")
	}
	return nil
}
