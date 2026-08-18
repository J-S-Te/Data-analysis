// Package aggregation 实现跨库同步 + T+1 预计算（设计方案 §5.3）。
// 骨架实现：dim_customer / dim_contract / dim_opportunity 维表镜像 + fct_contract_signing 预计算；
// 增量水印、Temporal 调度与其余 fct 表为 TODO（PoC V5 后按源库表结构扩展）。
package aggregation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

const (
	dictVersion   = "2026-07-28"
	statusRunning = "RUNNING"
	statusSuccess = "SUCCESS"
	statusFailed  = "FAILED"
)

// Runner 聚合任务执行器。
type Runner struct {
	aggDB   *gorm.DB
	sources map[string]string // subsystem_code -> 只读 DSN
	// effectiveStatuses 为"已生效"合同状态过滤（字典口径待确认；默认空=不过滤）
	effectiveStatuses []string
}

func NewRunner(aggDSN string, sources map[string]string) (*Runner, error) {
	db, err := gorm.Open(mysql.Open(aggDSN), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		return nil, err
	}
	return &Runner{aggDB: db, sources: sources}, nil
}

// SyncSource 聚合库 sync_source 记录。
type SyncSource struct {
	ID             string     `gorm:"column:id;primaryKey"`
	SubsystemCode  string     `gorm:"column:subsystem_code"`
	DBHost         string     `gorm:"column:db_host"`
	DBSchema       string     `gorm:"column:db_schema"`
	ReadAccountRef string     `gorm:"column:read_account_ref"`
	Enabled        bool       `gorm:"column:enabled"`
	LastWatermark  *time.Time `gorm:"column:last_watermark"`
	LastRunAt      *time.Time `gorm:"column:last_run_at"`
	LastStatus     string     `gorm:"column:last_status"`
	LastError      *string    `gorm:"column:last_error"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at"`
}

func (SyncSource) TableName() string { return "sync_source" }

// SyncJob 是管理员提交的单次同步请求，由 aggregation-worker 异步领取。
type SyncJob struct {
	ID            string     `gorm:"column:id;primaryKey"`
	SourceID      string     `gorm:"column:source_id"`
	SubsystemCode string     `gorm:"column:subsystem_code"`
	Status        string     `gorm:"column:status"`
	RequestedAt   time.Time  `gorm:"column:requested_at"`
	StartedAt     *time.Time `gorm:"column:started_at"`
	FinishedAt    *time.Time `gorm:"column:finished_at"`
	ErrorMessage  *string    `gorm:"column:error_message"`
}

func (SyncJob) TableName() string { return "sync_job" }

// EnsureSyncSources 幂等登记已配置 DSN 的同步源。
func (r *Runner) EnsureSyncSources(ctx context.Context) error {
	for code := range r.sources {
		var count int64
		if err := r.aggDB.Model(&SyncSource{}).Where("subsystem_code = ?", code).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			now := time.Now().UTC()
			if err := r.aggDB.Create(&SyncSource{
				ID: ulid(), SubsystemCode: code, DBHost: "source-secret", DBSchema: code,
				ReadAccountRef: code + ".read", Enabled: true,
				LastStatus: "PENDING", CreatedAt: now, UpdatedAt: now,
			}).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

// RunOnce 执行一轮完整同步 + 预计算（PoC V5 主线）。
func (r *Runner) RunOnce(ctx context.Context) error {
	start := time.Now().UTC()
	var runErrors []error
	for code, dsn := range r.sources {
		slog.Info("aggregation sync start", "source", code)
		r.mark(ctx, code, statusRunning, start, nil)
		var err error
		switch code {
		case "contract_management":
			err = r.syncContract(ctx, dsn)
		case "customer_and_opportunity":
			err = r.syncCRM(ctx, dsn)
		default:
			err = fmt.Errorf("unsupported source %s", code)
		}
		if err != nil {
			msg := err.Error()
			r.mark(ctx, code, statusFailed, start, &msg)
			slog.Error("aggregation sync failed", "source", code, "error", err)
			runErrors = append(runErrors, fmt.Errorf("sync source %s: %w", code, err))
			continue
		}
		r.mark(ctx, code, statusSuccess, start, nil)
		slog.Info("aggregation sync done", "source", code)
	}
	if err := r.PrecomputeContractSigning(ctx); err != nil {
		slog.Error("precompute failed", "error", err)
		runErrors = append(runErrors, fmt.Errorf("precompute contract signing: %w", err))
	}
	slog.Info("aggregation run completed", "elapsed", time.Since(start).String())
	return errors.Join(runErrors...)
}

// RunQueued 领取并执行管理员提交的手工同步请求。
// 通过条件更新抢占任务，多个 Worker 实例不会重复执行同一任务。
func (r *Runner) RunQueued(ctx context.Context) error {
	var jobs []SyncJob
	if err := r.aggDB.WithContext(ctx).Where("status = ?", "QUEUED").Order("requested_at ASC").Limit(10).Find(&jobs).Error; err != nil {
		return err
	}
	for _, job := range jobs {
		now := time.Now().UTC()
		result := r.aggDB.WithContext(ctx).Model(&SyncJob{}).
			Where("id = ? AND status = ?", job.ID, "QUEUED").
			Updates(map[string]interface{}{"status": "RUNNING", "started_at": now, "error_message": nil})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			continue
		}
		err := r.runSource(ctx, job.SubsystemCode, r.sources[job.SubsystemCode])
		finished := time.Now().UTC()
		updates := map[string]interface{}{"status": "SUCCESS", "finished_at": finished, "error_message": nil}
		if err != nil {
			message := err.Error()
			updates["status"] = "FAILED"
			updates["error_message"] = message
		}
		if updateErr := r.aggDB.WithContext(ctx).Model(&SyncJob{}).Where("id = ?", job.ID).Updates(updates).Error; updateErr != nil {
			return updateErr
		}
	}
	return nil
}

func (r *Runner) runSource(ctx context.Context, code, dsn string) error {
	if dsn == "" {
		return fmt.Errorf("source %s is not configured", code)
	}
	switch code {
	case "contract_management":
		return r.syncContract(ctx, dsn)
	case "customer_and_opportunity":
		return r.syncCRM(ctx, dsn)
	default:
		return fmt.Errorf("unsupported source %s", code)
	}
}

func (r *Runner) mark(ctx context.Context, code string, status string, runAt time.Time, errMsg *string) {
	updates := map[string]interface{}{"last_status": status, "last_run_at": runAt, "updated_at": time.Now().UTC()}
	if errMsg != nil {
		updates["last_error"] = *errMsg
	} else {
		updates["last_error"] = nil
	}
	_ = r.aggDB.Model(&SyncSource{}).Where("subsystem_code = ?", code).Updates(updates).Error
}

type connectionPoolCloser interface {
	Close() error
}

// closeSourceConnectionPool 释放单轮同步创建的源库连接池。
// 关闭失败不会覆盖本轮同步结果，但必须记录来源，便于定位资源泄漏。
func closeSourceConnectionPool(pool connectionPoolCloser, source string) {
	if err := pool.Close(); err != nil {
		slog.Warn("close aggregation source connection pool failed", "source", source, "error", err)
	}
}

// syncContract 合同库 → dim_contract（con_contract 全量快照，幂等 upsert）。
func (r *Runner) syncContract(ctx context.Context, dsn string) error {
	src, err := gorm.Open(mysql.Open(dsn), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		return err
	}
	sqlDB, err := src.DB()
	if err != nil {
		return fmt.Errorf("get contract source connection pool: %w", err)
	}
	defer closeSourceConnectionPool(sqlDB, "contract_management")

	type sourceRow struct {
		TenantID        string
		ID              string
		ContractNumber  string
		Title           string
		ContractType    string
		ServiceType     string
		Status          string
		StartDate       *time.Time
		EndDate         *time.Time
		CRMCustomerID   *uint64
		OpportunityID   *string
		AmountMinor     int64
		Currency        string
		OwnerUserID     string
		OwnerOrgID      *string
		OwnerIdentityID *string
		ProjectID       *string
		UpdatedAt       time.Time
	}
	rows := make([]sourceRow, 0, 500)
	if err := src.Table("con_contract").
		Select("tenant_id, id, contract_number, title, contract_type, service_type, status, start_date, end_date, crm_customer_id, opportunity_id, amount_minor, currency, owner_user_id, owner_org_id, owner_identity_id, project_id, updated_at").
		Find(&rows).Error; err != nil {
		return err
	}
	if len(rows) == 0 {
		slog.Info("contract source empty", "rows", 0)
		return nil
	}
	now := time.Now().UTC()
	for i := 0; i < len(rows); i += 200 {
		end := i + 200
		if end > len(rows) {
			end = len(rows)
		}
		batch := rows[i:end]
		for _, row := range batch {
			tenant := row.TenantID
			org := ""
			if row.OwnerOrgID != nil {
				org = *row.OwnerOrgID
			}
			identity := ""
			if row.OwnerIdentityID != nil {
				identity = *row.OwnerIdentityID
			}
			project := ""
			if row.ProjectID != nil {
				project = *row.ProjectID
			}
			var crmID *uint64
			if row.CRMCustomerID != nil {
				crmID = row.CRMCustomerID
			}
			if err := r.aggDB.Exec(`INSERT INTO dim_contract
(tenant_id, contract_id, contract_number, title, contract_type, service_type, status, start_date, end_date, crm_customer_id, opportunity_id, amount_minor, currency, owner_user_id, owner_org_id, owner_identity_id, project_id, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE title=VALUES(title), status=VALUES(status), start_date=VALUES(start_date), end_date=VALUES(end_date), crm_customer_id=VALUES(crm_customer_id), amount_minor=VALUES(amount_minor), owner_org_id=VALUES(owner_org_id), updated_at=VALUES(updated_at)`,
				tenant, row.ID, row.ContractNumber, row.Title, row.ContractType, row.ServiceType, row.Status,
				row.StartDate, row.EndDate, crmID, row.OpportunityID, row.AmountMinor, row.Currency,
				row.OwnerUserID, org, identity, project, row.UpdatedAt).Error; err != nil {
				return err
			}
		}
		slog.Info("dim_contract upserted", "rows", len(batch), "updated_at", now.Format(time.RFC3339))
	}
	return nil
}

// syncCRM 客户与商机库 → dim_customer / dim_opportunity。
func (r *Runner) syncCRM(ctx context.Context, dsn string) error {
	src, err := gorm.Open(mysql.Open(dsn), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		return err
	}
	sqlDB, err := src.DB()
	if err != nil {
		return fmt.Errorf("get CRM source connection pool: %w", err)
	}
	defer closeSourceConnectionPool(sqlDB, "customer_and_opportunity")

	// 客户
	type customerRow struct {
		ID           uint64
		TenantID     string
		CustomerNo   string
		Name         string
		CustomerType string
		Industry     string
		Region       string
		Status       string
		OwnerUserID  string
		OwnerOrgID   string
		DeletedAt    *time.Time
		UpdatedAt    time.Time
	}
	customers := make([]customerRow, 0, 500)
	if err := src.Table("crm_customers").
		Select("id, tenant_id, customer_no, name, customer_type, industry, region, status, owner_user_id, owner_org_id, deleted_at, updated_at").
		Find(&customers).Error; err != nil {
		return err
	}
	for i := 0; i < len(customers); i += 200 {
		end := i + 200
		if end > len(customers) {
			end = len(customers)
		}
		for _, row := range customers[i:end] {
			if err := r.aggDB.Exec(`INSERT INTO dim_customer
(tenant_id, customer_id, customer_no, customer_name, customer_type, industry, region, status, owner_user_id, owner_org_id, deleted_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE customer_name=VALUES(customer_name), customer_type=VALUES(customer_type), industry=VALUES(industry), region=VALUES(region), status=VALUES(status), deleted_at=VALUES(deleted_at), updated_at=VALUES(updated_at)`,
				row.TenantID, row.ID, row.CustomerNo, row.Name, row.CustomerType, row.Industry, row.Region,
				row.Status, row.OwnerUserID, row.OwnerOrgID, row.DeletedAt, row.UpdatedAt).Error; err != nil {
				return err
			}
		}
		slog.Info("dim_customer upserted", "rows", len(customers[i:end]))
	}
	// 商机
	type opportunityRow struct {
		ID               uint64
		TenantID         string
		OpportunityNo    string
		Name             string
		CustomerID       uint64
		Type             string
		Source           string
		ExpectedAmount   string
		ExpectedSignDate time.Time
		OwnerUserID      string
		OwnerOrgID       string
		CurrentStage     string
		OppStatus        string
		ContractRef      *string
		LostReason       *string
		StageChangedAt   time.Time
		DeletedAt        *time.Time
		UpdatedAt        time.Time
	}
	opportunities := make([]opportunityRow, 0, 500)
	if err := src.Table("crm_opportunities").
		Select("id, tenant_id, opportunity_no, name, customer_id, type, source, expected_amount, expected_sign_date, owner_user_id, owner_org_id, current_stage, opp_status, contract_ref, lost_reason, stage_changed_at, deleted_at, updated_at").
		Find(&opportunities).Error; err != nil {
		return err
	}
	for i := 0; i < len(opportunities); i += 200 {
		end := i + 200
		if end > len(opportunities) {
			end = len(opportunities)
		}
		for _, row := range opportunities[i:end] {
			if err := r.aggDB.Exec(`INSERT INTO dim_opportunity
(tenant_id, opportunity_id, opportunity_no, name, customer_id, type, source, expected_amount, expected_sign_date, owner_user_id, owner_org_id, current_stage, opp_status, contract_ref, lost_reason, stage_changed_at, deleted_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE name=VALUES(name), current_stage=VALUES(current_stage), opp_status=VALUES(opp_status), contract_ref=VALUES(contract_ref), lost_reason=VALUES(lost_reason), deleted_at=VALUES(deleted_at), updated_at=VALUES(updated_at)`,
				row.TenantID, row.ID, row.OpportunityNo, row.Name, row.CustomerID, row.Type, row.Source,
				row.ExpectedAmount, row.ExpectedSignDate, row.OwnerUserID, row.OwnerOrgID, row.CurrentStage,
				row.OppStatus, row.ContractRef, row.LostReason, row.StageChangedAt, row.DeletedAt, row.UpdatedAt).Error; err != nil {
				return err
			}
		}
		slog.Info("dim_opportunity upserted", "rows", len(opportunities[i:end]))
	}
	return nil
}

// PrecomputeContractSigning 预计算 fct_contract_signing（字典 1.1/1.2/1.3；转化率字段为占位）。
// 口径注记：period_month 取 start_date（"生效日"映射待确认）；状态过滤默认全量（EFFECTIVE_STATUSES env 可配）。
func (r *Runner) PrecomputeContractSigning(ctx context.Context) error {
	statusFilter := ""
	args := []interface{}{dictVersion}
	if len(r.effectiveStatuses) > 0 {
		placeholders := ""
		for i, s := range r.effectiveStatuses {
			if i > 0 {
				placeholders += ","
			}
			placeholders += "?"
			args = append(args, s)
		}
		statusFilter = " AND c.status IN (" + placeholders + ")"
	}
	query := `INSERT INTO fct_contract_signing
(tenant_id, period_month, region, customer_category, sales_director_id, sales_id, sign_amount_minor, contract_count, opportunity_count, won_contract_count, discount_rate_bucket, metric_dict_version)
SELECT c.tenant_id,
       DATE_FORMAT(c.start_date, '%Y-%m') AS period_month,
       COALESCE(cu.region, '未知') AS region,
       COALESCE(cu.customer_type, '未分类') AS customer_category,
       '' AS sales_director_id,
       c.owner_user_id AS sales_id,
       SUM(c.amount_minor) AS sign_amount_minor,
       COUNT(*) AS contract_count,
       0 AS opportunity_count,
       0 AS won_contract_count,
       '未知' AS discount_rate_bucket,
       ? AS metric_dict_version
FROM dim_contract c
LEFT JOIN dim_customer cu ON cu.tenant_id = c.tenant_id AND cu.customer_id = c.crm_customer_id
WHERE c.start_date IS NOT NULL` + statusFilter + `
GROUP BY c.tenant_id, period_month, region, customer_category, c.owner_user_id
ON DUPLICATE KEY UPDATE sign_amount_minor=VALUES(sign_amount_minor), contract_count=VALUES(contract_count)`
	if err := r.aggDB.Exec(query, args...).Error; err != nil {
		return err
	}
	var count int64
	if err := r.aggDB.Model(&struct{}{}).Table("fct_contract_signing").Count(&count).Error; err != nil {
		return err
	}
	slog.Info("fct_contract_signing rows", "count", count)
	return nil
}

func ulid() string {
	// 骨架：随机 ULID 风格 ID（正式实现对齐 platform 26 位 ULID 生成器）
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	b := make([]byte, 26)
	seed := time.Now().UnixNano()
	for i := range b {
		seed = seed*6364136223846793005 + 1442695040888963407
		b[i] = alphabet[uint64(seed>>33)%uint64(len(alphabet))]
	}
	return string(b)
}
