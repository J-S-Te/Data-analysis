package aggregation

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestQueuedApprovalFailureKeepsContractSuccess 验证审批失败不抹去已完成合同的成功凭据。
// 参数：t 为独立内存数据库测试上下文。
// 返回值：无；任务应失败，合同来源应成功，审批来源保留失败。
func TestQueuedApprovalFailureKeepsContractSuccess(t *testing.T) {
	r, job := queueTestRunner(t)
	job.SubsystemCode = "contract_management"
	if err := r.aggDB.Model(&SyncJob{}).Where("id = ?", job.ID).
		Update("subsystem_code", job.SubsystemCode).Error; err != nil {
		t.Fatal(err)
	}
	queueTestSyncSource(t, r, job.SubsystemCode, statusFailed, nil, nil)
	queueTestSyncSource(t, r, ContractApprovalSource, statusFailed, nil, nil)
	ctx := context.Background()
	lease, claimed, err := r.claimQueueJob(ctx, job, time.Minute)
	if err != nil || !claimed {
		t.Fatal(claimed, err)
	}
	partial := &ContractApprovalError{Err: errors.New("approval source unavailable")}
	if err = r.finishQueueJob(ctx, lease, partial, false); err != nil {
		t.Fatal(err)
	}
	source := queueTestSyncSourceRow(t, r, job.SubsystemCode)
	if source.LastStatus != statusSuccess || source.LastSuccessAt == nil {
		t.Fatalf("成功合同被审批失败覆盖: %+v", source)
	}
	if err = r.aggDB.First(&job, "id = ?", job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if job.Status != "FAILED" ||
		queueTestSyncSourceRow(t, r, ContractApprovalSource).LastStatus != statusFailed {
		t.Fatal("审批失败被掩盖")
	}
}
