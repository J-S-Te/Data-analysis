package aggregation

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// ContractApprovalSource 是合同审批投影通道在 sync_source 中的子系统编码。
// 它与 contract_management 共用 CONTRACT_READ_DSN（只读），但状态与失败互不影响：
// 审批表授权缺失时合同维表仍可成功同步，只是看板的 instances 口径被门禁挡住。
const ContractApprovalSource = "contract_approval"

// dimContractApprovalUpsert 把 con_approval_instance 投影到 dim_contract_approval。
//
// 关键设计点（不得改动）：守卫列必须是来源的 con_approval_instance.updated_at。
// dim_contract 的版本防护以 con_contract.updated_at 为版本，审批状态变化不会改合同行，
// 因此审批字段不能塞进 dim_contract（会被合同行守卫直接拒绝更新），必须独立成表并
// 自带来源时间。
var dimContractApprovalUpsert = guardedDimUpsert("dim_contract_approval",
	[]string{"tenant_id", "contract_id", "approval_id", "kind", "status", "started_at", "completed_at", "updated_at"},
	[]string{"kind", "status", "started_at", "completed_at"})

// contractApprovalSourceColumns 是 con_approval_instance 的只读投影列。
// 只取看板需要的最小集合：不镜像 nodes_json/runtime_state_json/applicant/reason 等审批内容，
// 也不读取 target_status（审批类型由 kind 表达）。
const contractApprovalSourceColumns = "tenant_id, id, contract_id, kind, status, created_at, completed_at, updated_at"

// contractApprovalRow 是 con_approval_instance 只读投影所需的最小列集合。
type contractApprovalRow struct {
	TenantID    string
	ID          string
	ContractID  string
	Kind        string
	Status      string
	CreatedAt   time.Time
	CompletedAt *time.Time
	UpdatedAt   time.Time
}

// ContractApprovalError 表示合同维表已同步成功，但审批投影刷新失败。
// 定时轮次据此保持 contract_management 的 SUCCESS 状态（合同数据是新鲜的），
// 同时把错误计入当轮并把 sync_source.contract_approval 记为 FAILED。
// 这样看板不会因为审批表授权缺失而误报合同数据陈旧。
type ContractApprovalError struct{ Err error }

// Error 返回带通道编码的失败摘要。
func (e *ContractApprovalError) Error() string {
	return "sync source " + ContractApprovalSource + ": " + e.Err.Error()
}

// Unwrap 支持 errors.Is/As 与聚合错误链。
func (e *ContractApprovalError) Unwrap() error { return e.Err }

// syncContractApproval 刷新合同审批投影（独立状态记录）。
// 参数：ctx 为许可/取消上下文，dsn 为合同只读连接（与合同维表共用）。
// 返回值：成功为 nil；源读取或聚合写入失败返回错误并只把本通道记为 FAILED。
func (r *Runner) syncContractApproval(ctx context.Context, dsn string) error {
	if dsn == "" {
		return fmt.Errorf("source %s is not configured", ContractApprovalSource)
	}
	start := time.Now().UTC()
	r.mark(ctx, ContractApprovalSource, statusRunning, start, nil)
	if err := r.syncContractApprovalFromSource(ctx, dsn); err != nil {
		msg := err.Error()
		r.mark(ctx, ContractApprovalSource, statusFailed, start, &msg)
		return err
	}
	r.mark(ctx, ContractApprovalSource, statusSuccess, start, nil)
	return nil
}

// syncContractApprovalFromSource 打开合同只读连接并写入审批投影。
// 参数：ctx 为许可/取消上下文，dsn 为合同只读连接串（不记录到日志）。
// 返回值：连接、读取或写入失败返回错误。
func (r *Runner) syncContractApprovalFromSource(ctx context.Context, dsn string) error {
	src, err := gorm.Open(mysql.Open(dsn), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		return err
	}
	sqlDB, err := src.DB()
	if err != nil {
		return fmt.Errorf("get contract approval source connection pool: %w", err)
	}
	defer closeSourceConnectionPool(sqlDB, ContractApprovalSource)
	return r.syncContractApprovalFrom(ctx, src.WithContext(ctx).Where("tenant_id = ?", r.tenantID))
}

// syncContractApprovalFrom 从已限定的源连接读取审批实例并 upsert 到 dim_contract_approval。
// 参数：ctx 为许可/取消上下文，src 为已按租户限定的 con_approval_instance 只读连接。
// 返回值：读取或写入失败返回错误；来源为空时是正常结果。
//
// 只投影看板需要的列（不镜像 nodes_json/runtime_state_json 等审批内容），
// 且只按租户读取，不跨租户聚合。
func (r *Runner) syncContractApprovalFrom(ctx context.Context, src *gorm.DB) error {
	total := 0
	err := scanSourceBatches(src, "con_approval_instance",
		contractApprovalSourceColumns,
		"id", func(row contractApprovalRow) string { return row.ID }, sourceReadBatchSize, func(rows []contractApprovalRow) error {
			for i := 0; i < len(rows); i += upsertChunkSize {
				end := i + upsertChunkSize
				if end > len(rows) {
					end = len(rows)
				}
				for _, row := range rows[i:end] {
					if err := r.checkLicense(ctx); err != nil {
						return err
					}
					if err := r.execAggregation(ctx, dimContractApprovalUpsert,
						row.TenantID, row.ContractID, row.ID, row.Kind, row.Status,
						row.CreatedAt, row.CompletedAt, row.UpdatedAt); err != nil {
						return err
					}
				}
				total += end - i
				slog.Info("dim_contract_approval upserted", "rows", end-i)
			}
			return nil
		})
	if err != nil {
		return err
	}
	if total == 0 {
		slog.Info("contract approval source empty", "rows", 0)
	}
	return nil
}
