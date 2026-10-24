package aggregation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrSyncLeaseLost = errors.New("sync job lease lost")

type queueLeaseKey struct{}
type queueLease struct {
	tenant, job, owner string
	generation         uint64
}

// 数据库时钟由宿主机时钟相互独立的工作器共享。租约
// 到期是排他的：过期持有者不能复活自身租约。
func queueDBNow(ctx context.Context, db *gorm.DB) (time.Time, error) {
	if db.Dialector.Name() == "sqlite" {
		var raw string
		if err := db.WithContext(ctx).Raw("SELECT strftime('%Y-%m-%d %H:%M:%f','now')").Scan(&raw).Error; err != nil {
			return time.Time{}, err
		}
		return time.Parse("2006-01-02 15:04:05.000", raw)
	}
	var now time.Time
	if err := db.WithContext(ctx).Raw("SELECT UTC_TIMESTAMP(3)").Scan(&now).Error; err != nil {
		return time.Time{}, err
	}
	return now.UTC(), nil
}

func (r *Runner) queueSettings() (time.Duration, time.Duration, time.Duration, error) {
	lease, beat, timeout := r.QueueLeaseDuration, r.QueueHeartbeatInterval, r.QueueTaskTimeout
	if lease == 0 {
		lease = 2 * time.Minute
	}
	if beat == 0 {
		beat = 30 * time.Second
	}
	if timeout == 0 {
		timeout = 30 * time.Minute
	}
	if lease <= 0 || beat <= 0 || lease < 3*beat || timeout <= 0 {
		return 0, 0, 0, errors.New("invalid sync queue lease/heartbeat/timeout configuration")
	}
	return lease, beat, timeout, nil
}

// ValidateQueueSettings 在启动任一聚合轮次前校验命令配置，
// 而非等普通来源运行后才失败。
func (r *Runner) ValidateQueueSettings() error {
	_, _, _, err := r.queueSettings()
	return err
}

func queueOwner() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func (r *Runner) claimQueueJob(ctx context.Context, job SyncJob, duration time.Duration) (queueLease, bool, error) {
	owner, err := queueOwner()
	if err != nil {
		return queueLease{}, false, err
	}
	var claimed bool
	lease := queueLease{r.tenantID, job.ID, owner, job.LeaseGeneration + 1}
	err = r.aggDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current SyncJob
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id=? AND id=?", r.tenantID, job.ID).First(&current).Error; err != nil {
			return err
		}
		now, err := queueDBNow(ctx, tx)
		if err != nil {
			return err
		}
		if current.LeaseGeneration != job.LeaseGeneration {
			return nil
		}
		queued := current.Status == "QUEUED"
		expired := current.Status == "RUNNING" && current.LeaseOwner != nil && current.LeaseExpiresAt != nil && !current.LeaseExpiresAt.After(now)
		if !queued && !expired {
			return nil
		}
		result := tx.Model(&SyncJob{}).Where("tenant_id=? AND id=? AND lease_generation=? AND status=?", r.tenantID, job.ID, job.LeaseGeneration, current.Status).
			Updates(map[string]any{"status": "RUNNING", "started_at": now, "finished_at": nil, "error_message": nil, "active_key": "ACTIVE", "lease_owner": owner, "lease_generation": gorm.Expr("lease_generation + 1"), "lease_expires_at": now.Add(duration)})
		claimed = result.RowsAffected == 1
		return result.Error
	})
	return lease, claimed, err
}

func (r *Runner) checkQueueLease(ctx context.Context, db *gorm.DB, lock bool) error {
	l, ok := ctx.Value(queueLeaseKey{}).(queueLease)
	if !ok {
		return nil
	}
	if l.tenant != r.tenantID {
		return ErrSyncLeaseLost
	}
	var job SyncJob
	q := db.WithContext(ctx).Where("tenant_id = ? AND id = ? AND status = 'RUNNING' AND lease_owner = ? AND lease_generation = ?", l.tenant, l.job, l.owner, l.generation)
	if lock {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	if err := q.First(&job).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrSyncLeaseLost
		}
		return err
	}
	now, err := queueDBNow(ctx, db)
	if err != nil {
		return err
	}
	if job.LeaseExpiresAt == nil || !job.LeaseExpiresAt.After(now) {
		return ErrSyncLeaseLost
	}
	return nil
}

// 每次队列聚合写入均在租约行锁的同一事务内隔离保护。
// 新持有者接管后，迟到旧工作器不能再发布。
func (r *Runner) aggregationWrite(ctx context.Context, write func(*gorm.DB) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.aggDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := r.checkQueueLease(ctx, tx, true); err != nil {
			return err
		}
		if r.LicenseCheck != nil {
			if err := r.LicenseCheck(ctx); err != nil {
				return err
			}
		}
		if err := write(tx); err != nil {
			return err
		}
		// Publication may be long: fence again immediately before commit while
		// retaining queue -> projection lock order established above.
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.checkQueueLease(ctx, tx, false); err != nil {
			return err
		}
		if r.LicenseCheck != nil {
			return r.LicenseCheck(ctx)
		}
		return nil
	})
}

func (r *Runner) execAggregation(ctx context.Context, query string, args ...any) error {
	return r.aggregationWrite(ctx, func(tx *gorm.DB) error { return tx.Exec(query, args...).Error })
}

func (r *Runner) renewQueueLease(ctx context.Context, l queueLease, duration time.Duration) error {
	// 读取数据库时间前先加锁，避免等待聚合写入的时间
	// 使续租使用过期的加锁前时间戳获得授权。
	return r.aggDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		leaseCtx := context.WithValue(ctx, queueLeaseKey{}, l)
		if err := r.checkQueueLease(leaseCtx, tx, true); err != nil {
			return err
		}
		now, err := queueDBNow(ctx, tx)
		if err != nil {
			return err
		}
		result := tx.Model(&SyncJob{}).Where("tenant_id=? AND id=? AND status='RUNNING' AND lease_owner=? AND lease_generation=?", l.tenant, l.job, l.owner, l.generation).Update("lease_expires_at", now.Add(duration))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrSyncLeaseLost
		}
		return nil
	})
}

func (r *Runner) finishQueueJob(ctx context.Context, l queueLease, runErr error, pause bool) error {
	return r.aggDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		leaseCtx := context.WithValue(ctx, queueLeaseKey{}, l)
		if err := r.checkQueueLease(leaseCtx, tx, true); err != nil {
			return err
		}
		now, err := queueDBNow(ctx, tx)
		if err != nil {
			return err
		}
		updates := map[string]any{"status": "SUCCESS", "finished_at": now, "error_message": nil, "active_key": nil, "lease_owner": nil, "lease_expires_at": nil}
		if pause {
			updates["status"] = "QUEUED"
			updates["started_at"] = nil
			updates["finished_at"] = nil
			updates["active_key"] = "ACTIVE"
		} else if runErr != nil {
			updates["status"] = "FAILED"
			updates["error_message"] = runErr.Error()
		}
		result := tx.Model(&SyncJob{}).Where("tenant_id=? AND id=? AND status='RUNNING' AND lease_owner=? AND lease_generation=?", l.tenant, l.job, l.owner, l.generation).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrSyncLeaseLost
		}
		return nil
	})
}

func (r *Runner) executeQueueJob(ctx context.Context, job SyncJob, l queueLease, duration, heartbeat, timeout time.Duration, run func(context.Context, string, string) error) error {
	jobCtx, cancel := context.WithTimeout(context.WithValue(ctx, queueLeaseKey{}, l), timeout)
	defer cancel()
	done, stopped := make(chan struct{}), make(chan struct{})
	leaseErrors := make(chan error, 1)
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(heartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-jobCtx.Done():
				return
			case <-ticker.C:
				beatCtx, beatCancel := context.WithTimeout(jobCtx, heartbeat)
				err := r.renewQueueLease(beatCtx, l, duration)
				beatCancel()
				if err != nil {
					leaseErrors <- err
					cancel()
					return
				}
			}
		}
	}()
	runErr := run(jobCtx, job.SubsystemCode, r.configuredSources()[job.SubsystemCode])
	close(done)
	<-stopped
	select {
	case err := <-leaseErrors:
		return fmt.Errorf("sync lease heartbeat: %w", err)
	default:
	}
	if errors.Is(runErr, ErrSyncLeaseLost) {
		return runErr
	}
	// Pause 保留有效槽位，以便续租或取消后幂等重放。
	pauseErr := jobCtx.Err()
	pause := pauseErr != nil
	if r.LicenseCheck != nil {
		if licenseErr := r.LicenseCheck(context.WithoutCancel(ctx)); licenseErr != nil {
			pause = true
			pauseErr = licenseErr
		}
	}
	finalCtx, finalCancel := context.WithTimeout(context.WithoutCancel(ctx), heartbeat)
	defer finalCancel()
	if err := r.finishQueueJob(finalCtx, l, runErr, pause); err != nil {
		return err
	}
	if pause {
		if runErr != nil {
			return runErr
		}
		return pauseErr
	}
	return nil
}

func (r *Runner) runLeasedQueue(ctx context.Context) error {
	if err := r.checkLicense(ctx); err != nil {
		return err
	}
	duration, heartbeat, timeout, err := r.queueSettings()
	if err != nil {
		return err
	}
	now, err := queueDBNow(ctx, r.aggDB)
	if err != nil {
		return err
	}
	var jobs []SyncJob
	if err = r.aggDB.WithContext(ctx).Where("tenant_id = ? AND (status = 'QUEUED' OR (status = 'RUNNING' AND lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL AND lease_expires_at <= ?))", r.tenantID, now).Order("requested_at ASC, id ASC").Limit(10).Find(&jobs).Error; err != nil {
		return err
	}
	for _, job := range jobs {
		if err = r.checkLicense(ctx); err != nil {
			return err
		}
		l, claimed, e := r.claimQueueJob(ctx, job, duration)
		if e != nil {
			return e
		}
		if !claimed {
			continue
		}
		if e = r.executeQueueJob(ctx, job, l, duration, heartbeat, timeout, r.runSource); e != nil {
			return e
		}
	}
	return nil
}
