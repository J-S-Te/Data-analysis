// Package aggregation 实现跨库同步 + T+1 预计算（设计方案 §5.3）。
// 只读来源同步、租户事实预计算和报告/回款快照。
package aggregation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

const (
	dictVersion   = "2026-07-28"
	statusRunning = "RUNNING"
	statusSuccess = "SUCCESS"
	statusFailed  = "FAILED"
	// upsertChunkSize 是写入侧每次提交给聚合库的行数（既有节奏，保持不变）。
	upsertChunkSize = 200
	// sourceReadBatchSize 是源表游标分批读取的每批行数（AUD-2026-030）：读侧不再
	// 整表一次性载入内存，最终行集合与全量读取等价。
	sourceReadBatchSize = 500
)

// Runner 是聚合任务执行器，封装聚合库连接与源系统同步配置。
type Runner struct {
	LicenseCheck      func(context.Context) error
	ProjectProjection ProjectProjectionOptions
	ProjectAPI        *APISyncRunner
	aggDB             *gorm.DB
	sources           map[string]string // subsystem_code -> 只读 DSN
	tenantID          string
	// effectiveStatuses 为"已生效"合同状态过滤（字典口径待确认；默认空=不过滤）
	effectiveStatuses []string
	// BusinessLocation 是财务快照业务日（as_of_date/90 天 cutoff/本月前缀）所在时区，
	// 由启动装配按 DASHBOARD_TIMEZONE 注入；为零值时按 UTC 处理（兼容既有测试与
	// 未配置的调用方）。它只影响日期边界，金额/账龄 SQL 语义与存储时间戳仍为 UTC。
	BusinessLocation       *time.Location
	QueueLeaseDuration     time.Duration
	QueueHeartbeatInterval time.Duration
	QueueTaskTimeout       time.Duration
	// RunOverlapGuard 控制“同租户/子系统存在未结束的其它轮次”时的 precompute 行为：
	// warn（默认）跳过 precompute + WARN，enforce 同样跳过并把重叠计为当轮错误。
	// 由启动装配按 AGG_RUN_OVERLAP_GUARD 注入；零值按 warn 处理。
	RunOverlapGuard RunOverlapGuard
	// precomputeSigning/precomputeConversion 允许测试观察重叠闸门是否放行 precompute，
	// 生产为 nil，走真实的 PrecomputeContractSigning/PrecomputeConversion。
	precomputeSigning    func(context.Context) error
	precomputeConversion func(context.Context) error
}

func (r *Runner) checkLicense(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.checkQueueLease(ctx, r.aggDB, false); err != nil {
		return err
	}
	if r.LicenseCheck != nil {
		return r.LicenseCheck(ctx)
	}
	return nil
}

// NewRunner 使用聚合库 DSN、租户和源系统配置初始化 Runner。
// aggDSN 为聚合数据库连接串；tenantID 是数据源配置的隔离边界；sources 映射 subsystem_code 到只读源 DSN；
// 成功时返回可复用的 *Runner，建立连接失败时返回错误并终止后续同步。
func NewRunner(aggDSN, tenantID string, sources map[string]string) (*Runner, error) {
	if tenantID == "" {
		return nil, errors.New("OIDC_TENANT_ID is required for aggregation source isolation")
	}
	// 租约时间戳是 UTC DATETIME 值，否则 MySQL DSN 时区
	// 会同时改变绑定与扫描时间，破坏共享数据库时钟。
	dsnConfig, err := mysqldriver.ParseDSN(aggDSN)
	if err != nil {
		return nil, errors.New("invalid aggregation database configuration")
	}
	dsnConfig.ParseTime = true
	dsnConfig.Loc = time.UTC
	db, err := gorm.Open(mysql.Open(dsnConfig.FormatDSN()), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		return nil, err
	}
	return &Runner{aggDB: db, tenantID: tenantID, sources: sources}, nil
}

// SyncSource 聚合库 sync_source 记录。
type SyncSource struct {
	TenantID       string     `gorm:"column:tenant_id"`
	ID             string     `gorm:"column:id;primaryKey"`
	SubsystemCode  string     `gorm:"column:subsystem_code"`
	DBHost         string     `gorm:"column:db_host"`
	DBSchema       string     `gorm:"column:db_schema"`
	ReadAccountRef string     `gorm:"column:read_account_ref"`
	Enabled        bool       `gorm:"column:enabled"`
	LastWatermark  *time.Time `gorm:"column:last_watermark"`
	LastRunAt      *time.Time `gorm:"column:last_run_at"`
	LastStatus     string     `gorm:"column:last_status"`
	// LastSuccessAt 是该通道最近一次成功的持久标记（不随后续失败清空）：
	// 看板 DASHBOARD_APPROVAL_SOURCE=instances 的升级门禁依赖“首次成功”证据。
	LastSuccessAt *time.Time `gorm:"column:last_success_at"`
	LastError     *string    `gorm:"column:last_error"`
	CreatedAt     time.Time  `gorm:"column:created_at"`
	UpdatedAt     time.Time  `gorm:"column:updated_at"`
}

// TableName 返回 sync_source 表名，供 GORM 映射与 SQL 查询使用。
func (SyncSource) TableName() string { return "sync_source" }

// SyncJob 是管理员提交的单次同步请求，由 aggregation-worker 异步领取。
type SyncJob struct {
	TenantID        string     `gorm:"column:tenant_id"`
	ID              string     `gorm:"column:id;primaryKey"`
	SourceID        string     `gorm:"column:source_id"`
	SubsystemCode   string     `gorm:"column:subsystem_code"`
	Status          string     `gorm:"column:status"`
	RequestedAt     time.Time  `gorm:"column:requested_at"`
	StartedAt       *time.Time `gorm:"column:started_at"`
	FinishedAt      *time.Time `gorm:"column:finished_at"`
	ErrorMessage    *string    `gorm:"column:error_message"`
	ActiveKey       *string    `gorm:"column:active_key"`
	LeaseOwner      *string    `gorm:"column:lease_owner"`
	LeaseGeneration uint64     `gorm:"column:lease_generation"`
	LeaseExpiresAt  *time.Time `gorm:"column:lease_expires_at"`
}

// TableName 返回 sync_job 表名，供 GORM 映射与同步任务状态更新使用。
func (SyncJob) TableName() string { return "sync_job" }

// EnsureSyncSources 幂等登记已配置只读 DSN 或项目投影 API 的同步源。
// 审批投影通道（contract_approval）与合同只读通道共用 CONTRACT_READ_DSN：
// 只有合同 DSN 配置时才登记，保证看板门禁读到的是“该通道真的跑过”的状态。
func (r *Runner) EnsureSyncSources(ctx context.Context) error {
	if err := r.checkLicense(ctx); err != nil {
		return err
	}
	codes := make([]string, 0, len(r.configuredSources())+1)
	for code := range r.configuredSources() {
		codes = append(codes, code)
	}
	if _, configured := r.sources["contract_management"]; configured {
		codes = append(codes, ContractApprovalSource)
	}
	for _, code := range codes {
		if err := r.checkLicense(ctx); err != nil {
			return err
		}
		if err := r.ensureSyncSource(ctx, code); err != nil {
			return err
		}
	}
	return nil
}

// ensureSyncSource 幂等登记单个同步源状态行。
// 参数：ctx 为许可/取消上下文，code 为子系统编码。
// 返回值：读取或创建失败返回错误；已存在时不改写状态。
func (r *Runner) ensureSyncSource(ctx context.Context, code string) error {
	var count int64
	if err := r.aggDB.WithContext(ctx).Model(&SyncSource{}).Where("tenant_id = ? AND subsystem_code = ?", r.tenantID, code).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	now := time.Now().UTC()
	return r.aggregationWrite(ctx, func(tx *gorm.DB) error {
		return tx.Create(&SyncSource{
			TenantID: r.tenantID, ID: ulid(), SubsystemCode: code, DBHost: "source-secret", DBSchema: code,
			ReadAccountRef: code + ".read", Enabled: true,
			LastStatus: "PENDING", CreatedAt: now, UpdatedAt: now,
		}).Error
	})
}

// RunOnce 执行一轮完整同步 + 预计算（PoC V5 主线）。
// ctx 用于透传取消信号；返回 errors.Join 聚合错误，多源失败时不短路；全部成功返回 nil。
// 每个来源在全量同步期间持有跨进程数据库锁；重叠实例跳过该来源而不是并发覆盖维表。
func (r *Runner) RunOnce(ctx context.Context) error {
	if err := r.checkLicense(ctx); err != nil {
		return err
	}
	return r.runScheduledSources(ctx, scheduledSyncLockerFor(r.aggDB))
}

// runScheduledSources 在来源级互斥锁下执行一轮定时同步。
// 参数：ctx 为许可/取消上下文，locker 为互斥实现（nil 时按聚合库方言选择）。
// 返回值：聚合错误；未获得锁的来源本轮跳过，不写同步状态或维表，避免旧批次覆盖新值。
func (r *Runner) runScheduledSources(ctx context.Context, locker scheduledSyncLocker) error {
	if err := r.checkLicense(ctx); err != nil {
		return err
	}
	if locker == nil {
		locker = scheduledSyncLockerFor(r.aggDB)
	}
	start := time.Now().UTC()
	// 每轮起止记录：用于跨实例轮次重叠检测（BIZ-DATA-08 残项 B）。
	// 记录失败只降级守卫（WARN），不影响本轮同步与 precompute。
	runRecorded := r.beginSyncRun(ctx, start)
	defer r.finishSyncRun(context.WithoutCancel(ctx), start)
	var runErrors []error
	sources := r.configuredSources()
	sourceOK := make(map[string]bool, len(sources))
	for code, dsn := range sources {
		if err := r.checkLicense(ctx); err != nil {
			return err
		}
		release, acquired, err := locker.TryAcquire(ctx, r.tenantID, code)
		if err != nil {
			// 锁不可用时宁可本轮不同步，也不能在无互斥保证下覆盖维表。
			runErrors = append(runErrors, fmt.Errorf("acquire sync lock for source %s: %w", code, err))
			continue
		}
		if !acquired {
			// 另一个进程正持有同一租户/来源的全量同步锁：本轮不读写该来源，
			// 避免迟到批次覆盖新值，也避免覆盖持锁进程的同步水位。
			slog.Warn("aggregation sync skipped: another process holds the source lock", "source", code)
			continue
		}
		sourceErr := func() error {
			defer func() {
				if releaseErr := release(); releaseErr != nil {
					slog.Warn("release aggregation sync lock failed", "source", code, "error", releaseErr)
				}
			}()
			slog.Info("aggregation sync start", "source", code)
			sourceCtx := ctx
			if isMetricSource(code) {
				sourceCtx = context.WithValue(ctx, metricRoundStartKey{}, start)
				if err := r.markMetricRound(sourceCtx, code, statusRunning, start, nil); err != nil {
					return err
				}
			} else {
				r.mark(ctx, code, statusRunning, start, nil)
			}
			if err := r.runSource(sourceCtx, code, dsn); err != nil {
				// 审批投影失败不影响已成功的合同维表：通道保持 SUCCESS，
				// 失败已写入 sync_source.contract_approval，错误仍计入当轮。
				var approvalErr *ContractApprovalError
				if errors.As(err, &approvalErr) {
					sourceOK[code] = true
					r.mark(ctx, code, statusSuccess, start, nil)
					slog.Warn("aggregation sync partial: contract approval projection failed", "source", code, "error", err)
					return err
				}
				msg := err.Error()
				if isMetricSource(code) {
					err = errors.Join(err, r.markMetricRound(ctx, code, statusFailed, start, err))
				} else {
					r.mark(ctx, code, statusFailed, start, &msg)
				}
				slog.Error("aggregation sync failed", "source", code, "error", err)
				return fmt.Errorf("sync source %s: %w", code, err)
			}
			sourceOK[code] = true
			if !isMetricSource(code) {
				r.mark(ctx, code, statusSuccess, start, nil)
			}
			slog.Info("aggregation sync done", "source", code)
			return nil
		}()
		if sourceErr != nil {
			runErrors = append(runErrors, sourceErr)
		}
	}
	// 从成功的来源预计算每个事实。无关的 CRM 或
	// 项目故障不得冻结合同签署事实。
	// 轮次重叠时跳过 precompute（派生数据，跳过不损失事实），避免混代次。
	if err := r.runPrecompute(ctx, sources, sourceOK, start, runRecorded); err != nil {
		runErrors = append(runErrors, err)
	}
	slog.Info("aggregation run completed", "elapsed", time.Since(start).String())
	return errors.Join(runErrors...)
}

// runPrecompute 在轮次重叠闸门下执行派生预计算。
// 参数：ctx 为许可/取消上下文，sources/sourceOK 为本轮来源与成功集合，
// runStartedAt 为本轮开始时间，runRecorded 表示 sync_run 是否已记录。
// 返回值：precompute 错误；重叠时按 AGG_RUN_OVERLAP_GUARD 决定是否返回错误。
func (r *Runner) runPrecompute(ctx context.Context, sources map[string]string, sourceOK map[string]bool, runStartedAt time.Time, runRecorded bool) error {
	signing, conversion := precomputeEligibility(sources, sourceOK)
	if !signing && !conversion {
		return nil
	}
	skip, guardErr := r.precomputeOverlapGuard(ctx, runStartedAt, runRecorded)
	if skip {
		return guardErr
	}
	var runErrors []error
	if signing {
		if err := r.runPrecomputeSigning(ctx); err != nil {
			slog.Error("precompute failed", "error", err)
			runErrors = append(runErrors, fmt.Errorf("precompute contract signing: %w", err))
		}
	}
	if conversion {
		if err := r.runPrecomputeConversion(ctx); err != nil {
			runErrors = append(runErrors, err)
		}
	}
	return errors.Join(runErrors...)
}

// runPrecomputeSigning 执行合同签署事实预计算；测试可注入替代实现观察闸门。
func (r *Runner) runPrecomputeSigning(ctx context.Context) error {
	if r.precomputeSigning != nil {
		return r.precomputeSigning(ctx)
	}
	return r.PrecomputeContractSigning(ctx)
}

// runPrecomputeConversion 执行商机转化事实预计算；测试可注入替代实现观察闸门。
func (r *Runner) runPrecomputeConversion(ctx context.Context) error {
	if r.precomputeConversion != nil {
		return r.precomputeConversion(ctx)
	}
	return r.PrecomputeConversion(ctx)
}

func precomputeEligibility(sources map[string]string, sourceOK map[string]bool) (signing, conversion bool) {
	_, contract := sources["contract_management"]
	_, crm := sources["customer_and_opportunity"]
	signing = contract && sourceOK["contract_management"]
	conversion = signing && crm && sourceOK["customer_and_opportunity"]
	return
}

// RunQueued 领取并执行管理员提交的手工同步请求。
// 通过条件更新抢占任务，多个 Worker 实例不会重复执行同一任务。
// ctx 用于控制数据库查询与调度循环；返回值仅在查询/更新失败时为非 nil，其余执行失败会进入作业行级失败快照并继续处理后续行。
func (r *Runner) RunQueued(ctx context.Context) error {
	return r.runLeasedQueue(ctx)
}

// runSource 在现有来源锁和租约内选择已配置通道，禁止失败后暗中切换来源。
// 参数：ctx 为当前执行权限，code 为固定来源编码，dsn 为可选只读数据库来源。
// 返回值：单来源执行错误；其他来源由轮次继续独立处理。
func (r *Runner) runSource(ctx context.Context, code, dsn string) error {
	if err := r.checkLicense(ctx); err != nil {
		return err
	}
	if code == "contract_metrics" || code == "project_metrics" || code == "finance_metrics" {
		return r.syncMetricFacts(ctx, code)
	}
	if code == "project_management" {
		return r.syncProject(ctx, dsn)
	}
	if r.ProjectAPI != nil {
		if code == "contract_dashboard" {
			return r.ProjectAPI.SyncContractDashboard(ctx)
		}
		if code == "project_dashboard" {
			return r.ProjectAPI.SyncProjectDashboard(ctx)
		}
	}
	if code == "settlement" && r.ProjectAPI != nil && r.ProjectAPI.options.SettlementFinanceURL != "" {
		return r.syncBusinessHTTP(ctx, "finance", r.ProjectAPI.options.SettlementFinanceURL,
			r.ProjectAPI.options.SettlementCredential)
	}
	if dsn == "" {
		return fmt.Errorf("source %s is not configured", code)
	}
	switch code {
	case "contract_management":
		if err := r.syncContract(ctx, dsn); err != nil {
			return err
		}
		// 审批投影挂在同一只读通道（定时与队列都经 runSource）：失败只降级审批口径
		// （instances 门禁），不撤销已成功的合同维表。包装成 ContractApprovalError，
		// 让定时轮次保持 contract_management 的 SUCCESS 状态。
		if err := r.syncContractApproval(ctx, dsn); err != nil {
			return &ContractApprovalError{Err: err}
		}
		return nil
	case "customer_and_opportunity":
		return r.syncCRM(ctx, dsn)
	case "settlement":
		return r.syncBusiness(ctx, code, dsn)
	default:
		return fmt.Errorf("unsupported source %s", code)
	}
}

// mark 更新本轮同步状态并仅在成功时推进持久成功凭据。
// 参数：ctx 为权限上下文，code/status 为来源和状态，runAt 为轮次开始，errMsg 为失败摘要。
// 返回值：无；状态更新受许可、租约和轮次时间防护。
func (r *Runner) mark(ctx context.Context, code string, status string, runAt time.Time, errMsg *string) {
	// DATETIME(3) 会四舍五入更细的小数；统一截断避免同轮终态比较落空。
	runAt = runAt.UTC().Truncate(time.Millisecond)
	updates := map[string]interface{}{"last_status": status, "last_run_at": runAt, "updated_at": time.Now().UTC()}
	if status == statusSuccess {
		// 持久化“曾经成功”：后续失败不清除，供看板 instances 口径的升级门禁使用。
		updates["last_success_at"] = time.Now().UTC()
	}
	if errMsg != nil {
		updates["last_error"] = *errMsg
	} else {
		updates["last_error"] = nil
	}
	_ = r.aggregationWrite(ctx, func(tx *gorm.DB) error {
		return tx.Model(&SyncSource{}).Where("tenant_id = ? AND subsystem_code = ? AND (last_run_at IS NULL OR last_run_at <= ?)", r.tenantID, code, runAt).Updates(updates).Error
	})
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

// scanSourceBatches 按主键升序游标分批读取源表行，避免整表一次性载入内存（AUD-2026-030）。
// pkColumn 必须是唯一且可全序比较的列（数值自增主键或业务字符串主键均可）：每批按
// WHERE pk > 上一批末行主键 ORDER BY pk ASC LIMIT batchSize 读取，读不满一批即为最后
// 一批；主键唯一保证全序且每行恰好被读取一次，最终行集合与整表全量读取等价（先后两次
// 批查询之间新提交的更高主键行会被顺带读入，不会漏读已存在行）。selectColumns 为显式
// 列清单，keyOf 从行结构提取主键；onBatch 在每批读取后立即执行既有落库逻辑，返回错误
// 时终止扫描并原样返回。src 为只读源库连接，连接池生命周期由调用方管理。
func scanSourceBatches[T any, K comparable](src *gorm.DB, table, selectColumns, pkColumn string, keyOf func(T) K, batchSize int, onBatch func([]T) error) error {
	var lastKey K
	for batch := 0; ; batch++ {
		rows := make([]T, 0, batchSize)
		query := src.Table(table).Select(selectColumns).Order(pkColumn + " ASC").Limit(batchSize)
		if batch > 0 {
			query = query.Where(pkColumn+" > ?", lastKey)
		}
		if err := query.Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		if err := onBatch(rows); err != nil {
			return err
		}
		if len(rows) < batchSize {
			return nil
		}
		lastKey = keyOf(rows[len(rows)-1])
	}
}

// dimContractUpsert、dimCustomerUpsert、dimOpportunityUpsert 是维表全量同步的语句。
// 它们用来源行 updated_at 作为版本防护：只有来源行不早于已存行时才覆盖业务列，
// 因此迟到或较旧的读取结果不会把较新的镜像数据回退。
//
// 说明：防护依赖 MySQL 对 ON DUPLICATE KEY UPDATE 赋值的从左到右求值，
// 所以 updated_at 自身必须放在赋值列表最后；业务列的守卫表达式读到的仍是旧行时间。
var (
	dimContractUpsert = guardedDimUpsert("dim_contract",
		[]string{"origin_kind", "tenant_id", "contract_id", "contract_number", "title", "contract_type",
			"service_type", "status", "start_date", "end_date", "crm_customer_id", "opportunity_id",
			"amount_minor", "currency", "owner_user_id", "owner_org_id", "owner_identity_id", "project_id", "updated_at"},
		[]string{"origin_kind", "contract_number", "title", "contract_type", "service_type", "status",
			"start_date", "end_date", "crm_customer_id", "opportunity_id", "currency", "amount_minor",
			"owner_user_id", "owner_identity_id", "project_id", "owner_org_id"})
	dimCustomerUpsert = guardedDimUpsert("dim_customer",
		[]string{"tenant_id", "customer_id", "customer_no", "customer_name", "customer_type", "industry",
			"region", "status", "owner_user_id", "owner_org_id", "deleted_at", "updated_at"},
		[]string{"customer_name", "customer_type", "industry", "region", "status", "owner_user_id",
			"owner_org_id", "deleted_at"})
	dimOpportunityUpsert = guardedDimUpsert("dim_opportunity",
		[]string{"tenant_id", "opportunity_id", "opportunity_no", "name", "customer_id", "type", "source",
			"expected_amount", "expected_sign_date", "owner_user_id", "owner_org_id", "current_stage",
			"opp_status", "contract_ref", "contract_id", "created_at", "lost_reason", "stage_changed_at",
			"deleted_at", "updated_at"},
		[]string{"name", "customer_id", "type", "source", "expected_amount", "expected_sign_date",
			"owner_user_id", "owner_org_id", "current_stage", "opp_status", "contract_ref", "contract_id",
			"created_at", "lost_reason", "stage_changed_at", "deleted_at"})
)

// guardedDimUpsert 构造带来源时间防护的维表 upsert 语句。
// 参数：table 为目标表，insertColumns 为插入列（最后一项必须是 updated_at），
// updateColumns 为冲突时允许覆盖的业务列（不含 updated_at）。
// 返回值：占位符与插入列同序的完整 SQL；updated_at 在赋值列表最后并以同样守卫更新。
func guardedDimUpsert(table string, insertColumns, updateColumns []string) string {
	placeholders := make([]string, len(insertColumns))
	for i := range insertColumns {
		placeholders[i] = "?"
	}
	assignments := make([]string, 0, len(updateColumns)+1)
	for _, column := range updateColumns {
		assignments = append(assignments, column+"=IF(VALUES(updated_at)>=updated_at,VALUES("+column+"),"+column+")")
	}
	assignments = append(assignments, "updated_at=IF(VALUES(updated_at)>=updated_at,VALUES(updated_at),updated_at)")
	return "INSERT INTO " + table + "\n(" + strings.Join(insertColumns, ", ") + ")\nVALUES (" +
		strings.Join(placeholders, ", ") + ")\nON DUPLICATE KEY UPDATE " + strings.Join(assignments, ", ")
}

// syncContract 将合同来源及来源类型同步到独立租户维表。
// 参数：ctx 为许可/取消上下文，dsn 为显式配置的合同只读连接。
// 返回值：同步成功为nil；源读取或聚合写入失败返回错误。
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
	return r.runDimensionSnapshot(ctx, src, "contract_management", func(snapshot *gorm.DB, run string) error {
		return r.syncContractSnapshot(ctx, snapshot, run)
	})
}

// syncContractSnapshot 分批写入同一完整合同快照，并暂存本轮主键集合。
// 参数：ctx 为许可/取消上下文，src 为限定租户的只读RR事务，run 为本轮令牌。
// 返回值：任一批读取、写入或暂存失败返回错误，禁止删除对账。
func (r *Runner) syncContractSnapshot(ctx context.Context, src *gorm.DB, run string) error {
	type sourceRow struct {
		OriginKind      string
		TenantID        string
		ID              string
		ContractNumber  *string
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
	now := time.Now().UTC()
	total := 0
	// AUD-2026-030：con_contract 全量改为主键（id，字符串业务主键）游标分批读取，
	// 每批内沿用既有 200 行 upsert 分块，落库结果与全量读取等价。
	if err := scanSourceBatches(src, "con_contract",
		"origin_kind, tenant_id, id, contract_number, title, contract_type, service_type, status, start_date, end_date, crm_customer_id, opportunity_id, amount_minor, currency, owner_user_id, owner_org_id, owner_identity_id, project_id, updated_at",
		"id", func(row sourceRow) string { return row.ID }, sourceReadBatchSize, func(rows []sourceRow) error {
			total += len(rows)
			for i := 0; i < len(rows); i += upsertChunkSize {
				end := i + upsertChunkSize
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
					if err := r.checkLicense(ctx); err != nil {
						return err
					}
					if err := r.execAggregation(ctx, dimContractUpsert,
						row.OriginKind, tenant, row.ID, row.ContractNumber, row.Title, row.ContractType, row.ServiceType, row.Status,
						row.StartDate, row.EndDate, crmID, row.OpportunityID, row.AmountMinor, row.Currency,
						row.OwnerUserID, org, identity, project, row.UpdatedAt); err != nil {
						return err
					}
				}
				slog.Info("dim_contract upserted", "rows", len(batch), "updated_at", now.Format(time.RFC3339))
			}
			keys := make([]string, 0, len(rows))
			for _, row := range rows {
				keys = append(keys, row.ID)
			}
			return r.stageDimensionKeys(ctx, run, "dim_contract", keys)
		}); err != nil {
		return err
	}
	if total == 0 {
		slog.Info("contract source empty", "rows", 0)
	}
	return nil
}

// syncCRM 在同一只读RR快照完整同步客户与商机后进行原子删除对账。
// 参数：ctx 为许可/取消上下文，dsn 为显式配置的CRM只读连接。
// 返回值：任一源表或删除对账失败返回错误，不按部分集合删除。
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
	return r.runDimensionSnapshot(ctx, src, "customer_and_opportunity", func(snapshot *gorm.DB, run string) error {
		return r.syncCRMSnapshot(ctx, snapshot, run)
	})
}

// syncCRMSnapshot 分批同步同一CRM快照中的客户和商机并暂存主键。
// 参数：ctx 为权限上下文，src 为限定租户的只读RR事务，run 为本轮令牌。
// 返回值：任一表读取、写入或暂存失败返回错误，不发布删除。
func (r *Runner) syncCRMSnapshot(ctx context.Context, src *gorm.DB, run string) error {
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
	// AUD-2026-030：crm_customers 全量改为主键（id，自增）游标分批读取，
	// 每批内沿用既有 200 行 upsert 分块，落库结果与全量读取等价。
	if err := scanSourceBatches(src, "crm_customers",
		"id, tenant_id, customer_no, name, customer_type, industry, region, status, owner_user_id, owner_org_id, deleted_at, updated_at",
		"id", func(row customerRow) uint64 { return row.ID }, sourceReadBatchSize, func(rows []customerRow) error {
			for i := 0; i < len(rows); i += upsertChunkSize {
				end := i + upsertChunkSize
				if end > len(rows) {
					end = len(rows)
				}
				for _, row := range rows[i:end] {
					if err := r.checkLicense(ctx); err != nil {
						return err
					}
					if err := r.execAggregation(ctx, dimCustomerUpsert,
						row.TenantID, row.ID, row.CustomerNo, row.Name, row.CustomerType, row.Industry, row.Region,
						row.Status, row.OwnerUserID, row.OwnerOrgID, row.DeletedAt, row.UpdatedAt); err != nil {
						return err
					}
				}
				slog.Info("dim_customer upserted", "rows", len(rows[i:end]))
			}
			keys := make([]string, 0, len(rows))
			for _, row := range rows {
				keys = append(keys, strconv.FormatUint(row.ID, 10))
			}
			return r.stageDimensionKeys(ctx, run, "dim_customer", keys)
		}); err != nil {
		return err
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
		ContractID       *string
		CreatedAt        time.Time
		LostReason       *string
		StageChangedAt   time.Time
		DeletedAt        *time.Time
		UpdatedAt        time.Time
	}
	// AUD-2026-030：crm_opportunities 全量改为主键（id，自增）游标分批读取，
	// 每批内沿用既有 200 行 upsert 分块，落库结果与全量读取等价。
	if err := scanSourceBatches(src, "crm_opportunities",
		"id, tenant_id, opportunity_no, name, customer_id, type, source, expected_amount, expected_sign_date, owner_user_id, owner_org_id, current_stage, opp_status, contract_ref, contract_id, created_at, lost_reason, stage_changed_at, deleted_at, updated_at",
		"id", func(row opportunityRow) uint64 { return row.ID }, sourceReadBatchSize, func(rows []opportunityRow) error {
			for i := 0; i < len(rows); i += upsertChunkSize {
				end := i + upsertChunkSize
				if end > len(rows) {
					end = len(rows)
				}
				for _, row := range rows[i:end] {
					if err := r.checkLicense(ctx); err != nil {
						return err
					}
					if err := r.execAggregation(ctx, dimOpportunityUpsert,
						row.TenantID, row.ID, row.OpportunityNo, row.Name, row.CustomerID, row.Type, row.Source,
						row.ExpectedAmount, row.ExpectedSignDate, row.OwnerUserID, row.OwnerOrgID, row.CurrentStage,
						row.OppStatus, row.ContractRef, row.ContractID, row.CreatedAt, row.LostReason, row.StageChangedAt, row.DeletedAt, row.UpdatedAt); err != nil {
						return err
					}
				}
				slog.Info("dim_opportunity upserted", "rows", len(rows[i:end]))
			}
			keys := make([]string, 0, len(rows))
			for _, row := range rows {
				keys = append(keys, strconv.FormatUint(row.ID, 10))
			}
			return r.stageDimensionKeys(ctx, run, "dim_opportunity", keys)
		}); err != nil {
		return err
	}
	return nil
}

// PrecomputeContractSigning 仅按普通合同原生效日预计算签约事实。
// 参数：ctx 为许可和取消上下文。
// 返回值：成功为nil；历史/未知来源不计入，数据库失败返回错误。
func (r *Runner) PrecomputeContractSigning(ctx context.Context) error {
	if err := r.checkLicense(ctx); err != nil {
		return err
	}
	statusFilter := ""
	args := []interface{}{dictVersion, r.tenantID}
	statuses := r.effectiveStatuses
	if len(statuses) == 0 {
		statuses = []string{"active", "in_progress", "pending_pay", "completed"}
	}
	if len(statuses) > 0 {
		placeholders := ""
		for i, s := range statuses {
			if i > 0 {
				placeholders += ","
			}
			placeholders += "?"
			args = append(args, s)
		}
		statusFilter = " AND c.status IN (" + placeholders + ")"
	}
	query := `INSERT INTO fct_contract_signing
(tenant_id, period_month, region, customer_category, sales_director_id, sales_id, sign_amount_minor, currency, contract_count, opportunity_count, won_contract_count, discount_rate_bucket, metric_dict_version)
SELECT c.tenant_id,
       DATE_FORMAT(c.start_date, '%Y-%m') AS period_month,
       COALESCE(cu.region, '未知') AS region,
       COALESCE(cu.customer_type, '未分类') AS customer_category,
       '' AS sales_director_id,
       c.owner_user_id AS sales_id,
       SUM(c.amount_minor) AS sign_amount_minor,
       c.currency AS currency,
       COUNT(*) AS contract_count,
       0 AS opportunity_count,
       0 AS won_contract_count,
       '未知' AS discount_rate_bucket,
       ? AS metric_dict_version
FROM dim_contract c
LEFT JOIN dim_customer cu ON cu.tenant_id = c.tenant_id AND cu.customer_id = c.crm_customer_id
WHERE c.tenant_id = ? AND c.origin_kind = 'NORMAL' AND c.start_date IS NOT NULL` + statusFilter + `
GROUP BY c.tenant_id, period_month, region, customer_category, c.owner_user_id, c.currency`
	if err := r.checkLicense(ctx); err != nil {
		return err
	}
	if err := r.aggregationWrite(ctx, func(tx *gorm.DB) error {
		if err := tx.Exec("DELETE FROM fct_contract_signing WHERE tenant_id = ?", r.tenantID).Error; err != nil {
			return err
		}
		return tx.Exec(query, args...).Error
	}); err != nil {
		return err
	}
	var count int64
	if err := r.aggDB.WithContext(ctx).Table("fct_contract_signing").Where("tenant_id = ?", r.tenantID).Count(&count).Error; err != nil {
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

// configuredSources 为定时和手工队列返回同一组已配置来源。
// 参数：无。
// 返回值：不含凭据的来源选择映射；API通道使用空DSN并由固定路由执行。
func (r *Runner) configuredSources() map[string]string {
	sources := make(map[string]string, len(r.sources)+1)
	for code, dsn := range r.sources {
		sources[code] = dsn
	}
	if r.ProjectProjection.URL != "" {
		sources["project_management"] = r.sources["project_management"]
	}
	if r.ProjectAPI != nil {
		for code := range r.metricSources() {
			sources[code] = ""
		}
		if r.ProjectAPI.options.ContractInternalURL != "" {
			sources["contract_dashboard"] = ""
		}
		if r.ProjectAPI.options.ProjectInternalURL != "" {
			sources["project_dashboard"] = ""
		}
		if r.ProjectAPI.options.ProjectReportURL != "" {
			sources["project_management"] = r.sources["project_management"]
		}
		if r.ProjectAPI.options.SettlementFinanceURL != "" {
			sources["settlement"] = r.sources["settlement"]
		}
	}
	return sources
}
