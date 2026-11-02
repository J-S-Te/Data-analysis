package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"github.com/unified-identity-auth-platform/data-analysis/internal/aggregation"
)

// ApprovalSource 是看板“审批中”计数的口径。
type ApprovalSource string

const (
	// ApprovalSourceStatus 用合同镜像的源状态 status='pending' 近似“审批中”。
	// 这是历史口径，会漏计合同仍为 active 的变更审批。
	ApprovalSourceStatus ApprovalSource = "status"
	// ApprovalSourceInstances 用 dim_contract_approval 中存在 status='running' 的
	// 审批实例判定“审批中”，与合同系统的权威口径一致（含 kind=status_change）。
	// 只有 contract_approval 同步通道首次 SUCCESS 之后才允许启用（fail-closed 门禁）。
	ApprovalSourceInstances ApprovalSource = "instances"
)

// ErrApprovalSourceNotReady 表示 DASHBOARD_APPROVAL_SOURCE=instances 但审批投影通道
// 尚未成功过任何一轮：此时切 instances 会让审批中从“近似 >0”退化成恒 0，
// 因此按硬校验拒绝（启动失败 / 运行期返回错误），而不是静默返回 0。
var ErrApprovalSourceNotReady = errors.New("DASHBOARD_APPROVAL_SOURCE=instances requires sync_source." + aggregation.ContractApprovalSource + " to have succeeded at least once; keep status until the approval channel succeeds")

// ParseApprovalSource 解析 DASHBOARD_APPROVAL_SOURCE。
// 参数：raw 为环境变量原值（大小写与首尾空白不敏感），空值取默认 status。
// 返回值：归一化口径；非法值返回错误，由启动装配失败关闭（不静默回退）。
func ParseApprovalSource(raw string) (ApprovalSource, error) {
	switch normalized := ApprovalSource(strings.ToLower(strings.TrimSpace(raw))); normalized {
	case "":
		return ApprovalSourceStatus, nil
	case ApprovalSourceStatus, ApprovalSourceInstances:
		return normalized, nil
	default:
		return "", errors.New("DASHBOARD_APPROVAL_SOURCE must be status or instances")
	}
}

// normalizeApprovalSource 把零值统一成 status，保证仓储默认口径不变。
func normalizeApprovalSource(source ApprovalSource) ApprovalSource {
	if source == "" {
		return ApprovalSourceStatus
	}
	return source
}

// ApprovalInstancesReady 报告 contract_approval 通道是否曾经成功过一轮。
// 参数：ctx 为查询上下文，db 为聚合库连接，tenantID 为租户。
// 返回值：曾经成功为 true；last_status/last_success_at 都未证明成功时为 false。
//
// 使用 last_success_at（000031 新增）而不是只看 last_status：后者会被下一轮失败覆盖，
// 无法表达“首次成功后即可切换”的语义。两列任一证明成功都算 ready，兼容升级前已成功
// 但尚未写新列的旧库状态。
func ApprovalInstancesReady(ctx context.Context, db *gorm.DB, tenantID string) (bool, error) {
	var count int64
	err := db.WithContext(ctx).Table("sync_source").
		Where("tenant_id = ? AND subsystem_code = ?", tenantID, aggregation.ContractApprovalSource).
		Where("last_status = ? OR last_success_at IS NOT NULL", "SUCCESS").
		Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// ValidateApprovalSource 校验审批口径在当前库状态可用（启动期硬校验）。
// 参数：ctx 为查询上下文，db 为聚合库连接，tenantID 为租户，source 为配置口径。
// 返回值：status 或无实例口径返回 nil；instances 且通道从未成功时返回 ErrApprovalSourceNotReady。
func ValidateApprovalSource(ctx context.Context, db *gorm.DB, tenantID string, source ApprovalSource) error {
	if normalizeApprovalSource(source) != ApprovalSourceInstances {
		return nil
	}
	ready, err := ApprovalInstancesReady(ctx, db, tenantID)
	if err != nil {
		return fmt.Errorf("check %s sync channel: %w", aggregation.ContractApprovalSource, err)
	}
	if !ready {
		return ErrApprovalSourceNotReady
	}
	return nil
}

// approvalCountExpr 返回 scopedContract 的“审批中”计数表达式。
// 参数：无（口径来自仓储配置）。
// 返回值：instances 口径用 dim_contract_approval 的 EXISTS(running)；
// 其余用源状态 status='pending'（历史口径，行为不变）。
//
// EXISTS 子查询按 dim_contract 的 tenant_id/contract_id 关联，租户隔离与
// 组织/个人范围收窄都在 dim_contract 侧完成，不会跨租户计数。
func (repository *GORMRepository) approvalCountExpr() string {
	if normalizeApprovalSource(repository.approvalSource) != ApprovalSourceInstances {
		return "COALESCE(SUM(CASE WHEN status='pending' THEN 1 ELSE 0 END),0) AS approval_contracts"
	}
	return `COALESCE(SUM(CASE WHEN EXISTS (SELECT 1 FROM dim_contract_approval a
			WHERE a.tenant_id = dim_contract.tenant_id AND a.contract_id = dim_contract.contract_id AND a.status = 'running')
			THEN 1 ELSE 0 END),0) AS approval_contracts`
}
