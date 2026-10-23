package aggregation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type LegacyRecoveryInput struct {
	JobIDs   []string
	Operator string
	// VerifyStopped 必须在调用方持有现有部署锁时，重新验证
	// 完整部署边界。证据仅含元数据。
	VerifyStopped func(context.Context) ([]byte, error)
}

type syncRecoveryAudit struct {
	ID             string `gorm:"primaryKey"`
	TenantID       string
	JobID          string
	OperatorID     string
	EvidenceSHA256 string
	RecoveredAt    time.Time
}

func (syncRecoveryAudit) TableName() string { return "sync_job_recovery_audit" }

func (r *Runner) legacyJobs(ctx context.Context, tx *gorm.DB, ids []string, lock bool) ([]SyncJob, error) {
	if r.tenantID == "" || len(ids) == 0 || len(ids) > 100 {
		return nil, errors.New("explicit tenant and 1 to 100 job IDs required")
	}
	jobs := make([]SyncJob, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			return nil, errors.New("invalid legacy sync job IDs")
		}
		seen[id] = true
		var job SyncJob
		q := tx.WithContext(ctx).Where("tenant_id=? AND id=? AND status='RUNNING' AND lease_owner IS NULL AND lease_generation=0 AND lease_expires_at IS NULL", r.tenantID, id)
		if lock {
			q = q.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := q.First(&job).Error; err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

// PreviewLegacySyncJobs 不执行恢复或审计写入。现代持有者及
// 异常状态或代次均安全拒绝，不能静默重置。
func (r *Runner) PreviewLegacySyncJobs(ctx context.Context, ids []string) ([]SyncJob, error) {
	return r.legacyJobs(ctx, r.aggDB, ids, false)
}

func (r *Runner) RecoverLegacySyncJobs(ctx context.Context, in LegacyRecoveryInput) error {
	if in.Operator == "" || len(in.Operator) > 128 || in.VerifyStopped == nil {
		return errors.New("operator and stopped-worker verifier required")
	}
	return r.aggDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		jobs, err := r.legacyJobs(ctx, tx, in.JobIDs, true)
		if err != nil {
			return err
		}
		evidence, err := in.VerifyStopped(ctx)
		if err != nil {
			return err
		}
		if !json.Valid(evidence) {
			return errors.New("invalid stopped-worker evidence")
		}
		digest := sha256.Sum256(evidence)
		now, err := queueDBNow(ctx, tx)
		if err != nil {
			return err
		}
		for _, job := range jobs {
			result := tx.Model(&SyncJob{}).Where("tenant_id=? AND id=? AND status='RUNNING' AND lease_owner IS NULL AND lease_generation=0 AND lease_expires_at IS NULL", r.tenantID, job.ID).Updates(map[string]any{"status": "QUEUED", "started_at": nil, "finished_at": nil, "error_message": nil, "active_key": "ACTIVE"})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrSyncLeaseLost
			}
			id, err := queueOwner()
			if err != nil {
				return err
			}
			if err = tx.Create(&syncRecoveryAudit{ID: id, TenantID: r.tenantID, JobID: job.ID, OperatorID: in.Operator, EvidenceSHA256: hex.EncodeToString(digest[:]), RecoveredAt: now}).Error; err != nil {
				return err
			}
		}
		// 提交前立即重检：边界重启或变化时同时回滚
		// 恢复及审计。调用方始终持有部署锁。
		again, err := in.VerifyStopped(ctx)
		if err != nil {
			return err
		}
		if sha256.Sum256(again) != digest {
			return errors.New("stopped-worker boundary changed during recovery")
		}
		return nil
	})
}
