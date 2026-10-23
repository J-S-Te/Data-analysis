package aggregation

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const ProjectProjectionCode = "project"

var ErrProjectProjectionSuperseded = errors.New("project projection attempt superseded")

type ProjectProjectionOptions struct {
	URL                                 string
	HTTPTimeout                         time.Duration
	MaxProjects, MaxBytes, MaxLineBytes int
}
type ProjectProjection struct {
	Type              string    `json:"type" gorm:"-"`
	TenantID          string    `json:"tenant_id" gorm:"column:tenant_id;primaryKey"`
	SnapshotID        string    `json:"snapshot_id" gorm:"column:snapshot_id"`
	ID                string    `json:"id" gorm:"column:project_id;primaryKey"`
	Name              string    `json:"name"`
	Customer          string    `json:"customer"`
	CustomerID        string    `json:"customer_id"`
	Contract          string    `json:"contract"`
	ContractID        string    `json:"contract_id"`
	Category          string    `json:"category"`
	Team              string    `json:"team"`
	Manager           string    `json:"manager"`
	ManagerIdentityID string    `json:"manager_identity_id"`
	OwnerOrgID        string    `json:"owner_org_id"`
	OwnerIdentityID   string    `json:"owner_identity_id"`
	Status            string    `json:"status"`
	Progress          int       `json:"progress"`
	Risk              bool      `json:"risk"`
	InFlight          bool      `json:"in_flight"`
	ServiceItems      int       `json:"service_items"`
	CreatedAt         time.Time `json:"created_at" gorm:"autoCreateTime:false"`
	UpdatedAt         time.Time `json:"updated_at" gorm:"autoUpdateTime:false"`
	SourceUpdatedAt   time.Time `json:"source_updated_at"`
}

func (ProjectProjection) TableName() string { return "dim_project" }

type projectProjectionStage struct {
	ProjectProjection `gorm:"embedded"`
	RunID             string `gorm:"primaryKey"`
}

func (projectProjectionStage) TableName() string { return "project_projection_stage" }

type ProjectionAttempt struct {
	TenantID, RunID string
	Generation      uint64
}
type ProjectProjectionState struct {
	TenantID                  string `gorm:"primaryKey"`
	ProjectionCode            string `gorm:"primaryKey"`
	RunID                     string
	Generation                uint64
	Status                    string
	LastError                 string
	StartedAt                 *time.Time
	FinishedAt                *time.Time
	LastSuccessAt             *time.Time
	PublishedRunID            string
	PublishedSnapshotID       string
	PublishedSourceSnapshotAt *time.Time
	PublishedSourceUpdatedAt  *time.Time
	PublishedTotal            int
	PublishedChecksumSHA256   string
}

func (ProjectProjectionState) TableName() string { return "project_projection_state" }

type ProjectSnapshotSummary struct {
	TenantID, SnapshotID string
	SourceSnapshotAt     time.Time
	SourceUpdatedAt      *time.Time
	Total                int
	ChecksumSHA256       string
	verified             bool
}
type ProjectSnapshotSink interface {
	Stage(context.Context, []ProjectProjection) error
}

// All mutations use the same queue-lock -> license -> projection-lock order.
func (r *Runner) beginProjectProjection(ctx context.Context) (ProjectionAttempt, error) {
	var attempt ProjectionAttempt
	err := r.aggregationWrite(ctx, func(tx *gorm.DB) error {
		if !projectIdentifier(r.tenantID, 26) {
			return errors.New("invalid projection tenant")
		}
		seed := ProjectProjectionState{TenantID: r.tenantID, ProjectionCode: ProjectProjectionCode, Status: "UNKNOWN"}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&seed).Error; err != nil {
			return err
		}
		var state ProjectProjectionState
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id=? AND projection_code=?", r.tenantID, ProjectProjectionCode).First(&state).Error; err != nil {
			return err
		}
		if state.Generation == math.MaxUint64 {
			return errors.New("project generation exhausted")
		}
		runID, err := queueOwner()
		if err != nil {
			return err
		}
		now, err := queueDBNow(ctx, tx)
		if err != nil {
			return err
		}
		attempt = ProjectionAttempt{TenantID: r.tenantID, RunID: runID, Generation: state.Generation + 1}
		return tx.Model(&ProjectProjectionState{}).Where("tenant_id=? AND projection_code=?", r.tenantID, ProjectProjectionCode).Updates(map[string]any{"run_id": runID, "generation": attempt.Generation, "status": statusRunning, "last_error": "", "started_at": now, "finished_at": nil}).Error
	})
	if err != nil {
		return ProjectionAttempt{}, err
	}
	// Reclaim before allowing the caller to stage another snapshot. A bounded
	// backlog leaves this attempt empty and retryable instead of accumulating more.
	if err = r.reclaimProjectStaging(ctx, attempt); err != nil {
		return ProjectionAttempt{}, err
	}
	return attempt, nil
}

// Each batch rechecks license/lease and locks the authoritative generation. A
// superseded writer cannot race reclamation or add more rows after its fence.
func (r *Runner) reclaimProjectStaging(ctx context.Context, a ProjectionAttempt) error {
	const batchSize = 200
	for batch := 0; batch < 250; batch++ {
		count := 0
		err := r.aggregationWrite(ctx, func(tx *gorm.DB) error {
			if err := r.lockProjectAttempt(tx, a); err != nil {
				return err
			}
			var keys []projectProjectionStage
			if err := tx.Select("tenant_id, run_id, project_id").Where("tenant_id=? AND run_id<>?", a.TenantID, a.RunID).Order("run_id, project_id").Limit(batchSize).Find(&keys).Error; err != nil {
				return err
			}
			count = len(keys)
			byRun := map[string][]string{}
			for _, key := range keys {
				byRun[key.RunID] = append(byRun[key.RunID], key.ID)
			}
			for run, ids := range byRun {
				if err := tx.Where("tenant_id=? AND run_id=? AND project_id IN ?", a.TenantID, run, ids).Delete(&projectProjectionStage{}).Error; err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		if count < batchSize {
			return nil
		}
	}
	return errors.New("project staging reclamation backlog; retry required")
}
func (r *Runner) lockProjectAttempt(tx *gorm.DB, a ProjectionAttempt) error {
	if a.TenantID != r.tenantID || a.RunID == "" || a.Generation == 0 {
		return ErrProjectProjectionSuperseded
	}
	var state ProjectProjectionState
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id=? AND projection_code=?", a.TenantID, ProjectProjectionCode).First(&state).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrProjectProjectionSuperseded
	}
	if err != nil {
		return err
	}
	if state.RunID != a.RunID || state.Generation != a.Generation || state.Status != statusRunning {
		return ErrProjectProjectionSuperseded
	}
	return nil
}
func (r *Runner) stageProjectProjection(ctx context.Context, a ProjectionAttempt, rows []ProjectProjection) error {
	return r.aggregationWrite(ctx, func(tx *gorm.DB) error {
		if err := r.lockProjectAttempt(tx, a); err != nil {
			return err
		}
		if len(rows) > 200 {
			return errors.New("project staging batch limit")
		}
		if len(rows) == 0 {
			return nil
		}
		staged := make([]projectProjectionStage, 0, len(rows))
		last := ""
		for _, p := range rows {
			if err := validateProjectRow(p); err != nil {
				return err
			}
			if p.TenantID != a.TenantID || p.ID <= last {
				return errors.New("conflicting staging row")
			}
			last = p.ID
			staged = append(staged, projectProjectionStage{ProjectProjection: p, RunID: a.RunID})
		}
		if err := tx.Create(&staged).Error; err != nil {
			return err
		}
		var totals struct {
			Total        int64
			ServiceItems int64
			Snapshots    int64
		}
		if err := tx.Model(&projectProjectionStage{}).Select("COUNT(*) AS total, COALESCE(SUM(service_items),0) AS service_items, COUNT(DISTINCT snapshot_id) AS snapshots").Where("tenant_id=? AND run_id=?", a.TenantID, a.RunID).Scan(&totals).Error; err != nil {
			return err
		}
		if totals.Total > 50000 || totals.ServiceItems > maxProjectServiceItems || totals.Snapshots != 1 {
			return errors.New("project staging count or snapshot conflict")
		}
		return nil
	})
}

const projectInsertColumns = "tenant_id, snapshot_id, project_id, name, customer, customer_id, contract, contract_id, category, team, manager, manager_identity_id, owner_org_id, owner_identity_id, status, progress, risk, in_flight, service_items, created_at, updated_at, source_updated_at"

func (r *Runner) publishProjectProjection(ctx context.Context, a ProjectionAttempt, s ProjectSnapshotSummary) error {
	return r.aggregationWrite(ctx, func(tx *gorm.DB) error {
		if err := r.lockProjectAttempt(tx, a); err != nil {
			return err
		}
		if !s.verified || s.TenantID != a.TenantID || !projectIdentifier(s.SnapshotID, 128) || s.Total < 0 || s.Total > 50000 || s.SourceSnapshotAt.IsZero() {
			return errors.New("unverified project summary")
		}
		var total, matching int64
		q := tx.Model(&projectProjectionStage{}).Where("tenant_id=? AND run_id=?", a.TenantID, a.RunID)
		if err := q.Count(&total).Error; err != nil {
			return err
		}
		if err := tx.Model(&projectProjectionStage{}).Where("tenant_id=? AND run_id=? AND snapshot_id=?", a.TenantID, a.RunID, s.SnapshotID).Count(&matching).Error; err != nil {
			return err
		}
		if total != int64(s.Total) || matching != total {
			return errors.New("project staging summary mismatch")
		}
		if err := tx.Where("tenant_id=?", a.TenantID).Delete(&ProjectProjection{}).Error; err != nil {
			return err
		}
		if err := tx.Exec("INSERT INTO dim_project ("+projectInsertColumns+") SELECT "+projectInsertColumns+" FROM project_projection_stage WHERE tenant_id=? AND run_id=?", a.TenantID, a.RunID).Error; err != nil {
			return fmt.Errorf("publish project rows: %w", err)
		}
		now, err := queueDBNow(ctx, tx)
		if err != nil {
			return err
		}
		if err = tx.Model(&ProjectProjectionState{}).Where("tenant_id=? AND projection_code=?", a.TenantID, ProjectProjectionCode).Updates(map[string]any{"status": statusSuccess, "last_error": "", "finished_at": now, "last_success_at": now, "published_run_id": a.RunID, "published_snapshot_id": s.SnapshotID, "published_source_snapshot_at": s.SourceSnapshotAt, "published_source_updated_at": s.SourceUpdatedAt, "published_total": s.Total, "published_checksum_sha256": s.ChecksumSHA256}).Error; err != nil {
			return err
		}
		return tx.Where("tenant_id=? AND run_id=?", a.TenantID, a.RunID).Delete(&projectProjectionStage{}).Error
	})
}

// Unknown strings are reduced to a fixed code; transport bodies and errors may contain secrets.
func projectErrorCode(code string) string {
	switch code {
	case "INVALID_STREAM", "SOURCE_UNAVAILABLE", "SOURCE_TIMEOUT", "STAGING_FAILED", "PUBLISH_FAILED", "CANCELED":
		return code
	default:
		return "PROJECTION_FAILED"
	}
}
func (r *Runner) failProjectProjection(ctx context.Context, a ProjectionAttempt, errorCode string) error {
	return r.aggregationWrite(ctx, func(tx *gorm.DB) error {
		if err := r.lockProjectAttempt(tx, a); err != nil {
			return err
		}
		now, err := queueDBNow(ctx, tx)
		if err != nil {
			return err
		}
		if err = tx.Model(&ProjectProjectionState{}).Where("tenant_id=? AND projection_code=?", a.TenantID, ProjectProjectionCode).Updates(map[string]any{"status": statusFailed, "last_error": projectErrorCode(errorCode), "finished_at": now}).Error; err != nil {
			return err
		}
		return tx.Where("tenant_id=? AND run_id=?", a.TenantID, a.RunID).Delete(&projectProjectionStage{}).Error
	})
}
