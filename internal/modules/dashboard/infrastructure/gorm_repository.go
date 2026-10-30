// Package infrastructure 包含看板模块拥有的存储适配器，
// 仅属于数据分析，不是可复用子系统包。
package infrastructure

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"gorm.io/gorm"

	"github.com/unified-identity-auth-platform/data-analysis/internal/aggregation"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
)

// GORMRepository 从 MySQL 视图读取物化看板快照。
type GORMRepository struct{ db *gorm.DB }

// NewGORMRepository 构造看板持久化适配器。
func NewGORMRepository(db *gorm.DB) *GORMRepository { return &GORMRepository{db: db} }

// LatestContract 返回租户最新合同聚合。
func (repository *GORMRepository) LatestContract(ctx context.Context, tenantID string) (domain.ContractSnapshot, bool, error) {
	scope := scopeFor(ctx, tenantID)
	if scope.mode != "TENANT" {
		return repository.scopedContract(ctx, scope)
	}
	var row struct {
		TenantID           string `gorm:"column:tenant_id"`
		SnapshotAt         string `gorm:"column:snapshot_at"`
		TotalAmountMinor   int64  `gorm:"column:total_amount_minor"`
		CurrencyTotalsJSON []byte `gorm:"column:currency_totals_json"`
		TotalContracts     int64  `gorm:"column:total_contracts"`
		ApprovalContracts  int64  `gorm:"column:approval_contracts"`
		ActiveContracts    int64  `gorm:"column:active_contracts"`
		ExpiredContracts   int64  `gorm:"column:expired_contracts"`
		OpportunityCount   int64  `gorm:"column:opportunity_count"`
		WonContractCount   int64  `gorm:"column:won_contract_count"`
	}
	err := repository.db.WithContext(ctx).Table("api_contract_dashboard").
		Where("tenant_id = ?", tenantID).Order("snapshot_at DESC").First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return domain.ContractSnapshot{DiscountBuckets: map[string]int64{}}, false, nil
	}
	if err != nil {
		return domain.ContractSnapshot{}, false, err
	}
	var funnel struct {
		OpportunityCount int64
		WonContractCount int64
		FactCount        int64
	}
	if err := repository.db.WithContext(ctx).Table("fct_opportunity_conversion").Select("COALESCE(SUM(opportunity_count),0) opportunity_count, COALESCE(SUM(won_contract_count),0) won_contract_count, COUNT(*) fact_count").Where("tenant_id = ?", tenantID).Scan(&funnel).Error; err != nil {
		return domain.ContractSnapshot{}, false, err
	}
	buckets := map[string]int64{}
	currencyTotals := []domain.ContractCurrencyTotal{}
	if len(row.CurrencyTotalsJSON) > 0 {
		if err := json.Unmarshal(row.CurrencyTotalsJSON, &currencyTotals); err != nil {
			return domain.ContractSnapshot{}, false, err
		}
	}
	return domain.ContractSnapshot{
		TenantID: row.TenantID, SnapshotAt: row.SnapshotAt, TotalAmountMinor: row.TotalAmountMinor, CurrencyTotals: currencyTotals,
		// 全租户分支读取接口持久快照：snapshot_at 是该快照的写入时间，
		// computed_at 另列本次查询时间，既有 snapshot_at 语义保持不变。
		Freshness: application.BuildFreshness(application.FreshnessInput{
			PersistedSnapshot:   true,
			PersistedSnapshotAt: row.SnapshotAt,
		}, time.Now().UTC()),
		TotalContracts: row.TotalContracts, ApprovalContracts: row.ApprovalContracts,
		ActiveContracts: row.ActiveContracts, ExpiredContracts: row.ExpiredContracts,
		OpportunityCount: funnel.OpportunityCount, WonContractCount: funnel.WonContractCount, DiscountBuckets: buckets,
		ConversionAvailable:       funnel.FactCount > 0,
		ConversionBasis:           "有效商机创建月队列；分子为已签约且关联已生效合同的去重商机；排除作废/删除记录，不以合同数代替商机数。",
		DiscountUnavailableReason: "尚无权威标准报价及币种基准；商机预计金额与已审批报价不自动视为标准报价，不能计算商业折扣。",
	}, true, nil
}

// LatestProject 返回租户最新项目聚合。
func (repository *GORMRepository) LatestProject(ctx context.Context, tenantID string) (domain.ProjectSnapshot, bool, error) {
	scope := scopeFor(ctx, tenantID)
	if scope.mode != "TENANT" {
		return repository.scopedProject(ctx, scope)
	}
	var row struct {
		TenantID         string `gorm:"column:tenant_id"`
		SnapshotAt       string `gorm:"column:snapshot_at"`
		ProjectCount     int    `gorm:"column:project_count"`
		InFlightProjects int    `gorm:"column:in_flight_projects"`
		RiskProjects     int    `gorm:"column:risk_projects"`
		ServiceItems     int    `gorm:"column:service_items"`
		StatusCountsJSON string `gorm:"column:status_counts_json"`
	}
	err := repository.db.WithContext(ctx).Table("api_project_dashboard").
		Where("tenant_id = ?", tenantID).Order("snapshot_at DESC").First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return domain.ProjectSnapshot{StatusCounts: map[string]int{}}, false, nil
	}
	if err != nil {
		return domain.ProjectSnapshot{}, false, err
	}
	statusCounts := map[string]int{}
	if row.StatusCountsJSON != "" {
		if err := json.Unmarshal([]byte(row.StatusCountsJSON), &statusCounts); err != nil {
			return domain.ProjectSnapshot{}, false, fmt.Errorf("%w: %v", application.ErrInvalidProjectSnapshot, err)
		}
	}
	return domain.ProjectSnapshot{
		TenantID: row.TenantID, SnapshotAt: row.SnapshotAt,
		Freshness: application.BuildFreshness(application.FreshnessInput{
			PersistedSnapshot:   true,
			PersistedSnapshotAt: row.SnapshotAt,
		}, time.Now().UTC()),
		ProjectCount:     row.ProjectCount,
		InFlightProjects: row.InFlightProjects, RiskProjects: row.RiskProjects,
		ServiceItems: row.ServiceItems, StatusCounts: statusCounts,
	}, true, nil
}

// ListContracts 返回范围内普通合同的脱敏分页明细。
// 参数：ctx 含可信范围，tenantID 为租户，page/pageSize 为受限分页。
// 返回值：普通合同列表、总数及数据库错误；历史/未知来源被排除。
func (repository *GORMRepository) ListContracts(ctx context.Context, tenantID string, page, pageSize int) ([]domain.ContractDetail, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	query := repository.db.WithContext(ctx).Table("dim_contract").Where("origin_kind = 'NORMAL'").Where("tenant_id = ?", tenantID)
	query = applyContractScope(query, scopeFor(ctx, tenantID))
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []domain.ContractDetail
	err := query.Select("contract_number,title,status,amount_minor,currency,COALESCE(DATE_FORMAT(end_date,'%Y-%m-%d'),'') end_date").Order("updated_at DESC, contract_id DESC").Offset((page - 1) * pageSize).Limit(pageSize).Scan(&rows).Error
	return rows, total, err
}

// ListTrend 返回最近若干月的合同和项目趋势；缺失月份由前端按空值展示。
func (repository *GORMRepository) ListTrend(ctx context.Context, tenantID string, months int) ([]domain.TrendPoint, error) {
	if months < 1 || months > 24 {
		months = 12
	}
	scope := scopeFor(ctx, tenantID)
	if scope.mode != "TENANT" {
		return repository.scopedTrend(ctx, scope, months)
	}
	var contractFacts []struct {
		Period   string
		Currency string
		Amount   int64
		Count    int64
	}
	err := repository.db.WithContext(ctx).Table("fct_contract_signing f").
		Select("f.period_month period, f.currency, COALESCE(SUM(f.sign_amount_minor),0) amount, COALESCE(SUM(f.contract_count),0) count").
		Where("f.tenant_id = ?", tenantID).Group("f.period_month, f.currency").Order("f.period_month DESC").Limit(months * 8).Scan(&contractFacts).Error
	if err != nil {
		return nil, err
	}
	contractRows := trendRowsByCurrency(contractFacts, months)
	// 每轮聚合均写入项目快照。将 SQL 窗口
	// 限制到请求月份，且每月仅保留最新快照，
	// 避免把持续增长的表全部加载到 Go。
	var projects []struct {
		Period string `gorm:"column:period"`
		Count  int    `gorm:"column:project_count"`
	}
	projectSQL := `SELECT period, project_count FROM (
		SELECT DATE_FORMAT(snapshot_at, '%Y-%m') AS period, project_count,
		       ROW_NUMBER() OVER (PARTITION BY DATE_FORMAT(snapshot_at, '%Y-%m') ORDER BY snapshot_at DESC, id DESC) AS rn
		FROM api_project_dashboard
		WHERE tenant_id = ? AND snapshot_at >= DATE_SUB(UTC_TIMESTAMP(3), INTERVAL ? MONTH)
	) t WHERE rn = 1 ORDER BY period DESC LIMIT ?`
	if err := repository.db.WithContext(ctx).Raw(projectSQL, tenantID, months, months).Scan(&projects).Error; err != nil {
		return nil, err
	}
	projectByPeriod := make(map[string]int, len(projects))
	for _, item := range projects {
		if _, exists := projectByPeriod[item.Period]; !exists {
			projectByPeriod[item.Period] = item.Count
		}
	}
	rows := mergeTrendPoints(contractRows, projectByPeriod, months)
	return rows, nil
}

// applyContractScope 对普通合同使用与历史分栏相同的可信个人/组织边界。
// 参数：query 为租户已限定的查询，scope 为可信主体范围。
// 返回值：个人仅匹配owner_user_id，其余复用组织策略的安全查询。
func applyContractScope(query *gorm.DB, scope queryScope) *gorm.DB {
	if scope.mode == "SELF" {
		if scope.userID == "" {
			return query.Where("1=0")
		}
		return query.Where("owner_user_id=?", scope.userID)
	}
	return applyOrgScope(query, scope, "owner_org_id")
}

func applyOrgScope(query *gorm.DB, scope queryScope, column string) *gorm.DB {
	switch scope.mode {
	case "TENANT":
		return query
	case "ORG", "TEAM":
		if len(scope.orgUnitIDs) == 0 {
			return query.Where("1=0")
		}
		return query.Where(column+" IN ?", scope.orgUnitIDs)
	default:
		return query.Where("1=0")
	}
}

// scopedContract 按授权组织或团队统计普通合同。
// 参数：ctx 为读取上下文，scope 为已解析的可信范围。
// 返回值：范围快照、可用标记及读取错误；历史和未知来源不计入。
func (repository *GORMRepository) scopedContract(ctx context.Context, scope queryScope) (domain.ContractSnapshot, bool, error) {
	if len(scope.orgUnitIDs) == 0 && (scope.mode != "SELF" || scope.userID == "") {
		return domain.ContractSnapshot{DiscountBuckets: map[string]int64{}}, false, nil
	}
	var row struct {
		TotalAmountMinor  int64
		TotalContracts    int64
		ApprovalContracts int64
		ActiveContracts   int64
		ExpiredContracts  int64
		// 源业务行 updated_at 的最大值，用于 source_updated_at，不能拿查询时间顶替。
		SourceUpdatedAt *string `gorm:"column:source_updated_at"`
	}
	scoped := applyContractScope(repository.db.WithContext(ctx).Table("dim_contract").Where("origin_kind = 'NORMAL'").Where("tenant_id = ?", scope.tenantID), scope)
	err := scoped.Select(`COALESCE(SUM(amount_minor),0) AS total_amount_minor, COUNT(*) AS total_contracts,
			COALESCE(SUM(status IN ('APPROVING','PENDING_APPROVAL','APPROVAL')),0) AS approval_contracts,
			COALESCE(SUM(status IN ('ACTIVE','EFFECTIVE','SIGNED')),0) AS active_contracts,
			COALESCE(SUM(status='EXPIRED'),0) AS expired_contracts,
			MAX(updated_at) AS source_updated_at`).Scan(&row).Error
	if err != nil {
		return domain.ContractSnapshot{}, false, err
	}
	var currencyRows []struct {
		Currency    string
		AmountMinor int64
		Contracts   int64
	}
	if err := applyContractScope(repository.db.WithContext(ctx).Table("dim_contract").Where("origin_kind = 'NORMAL'").Where("tenant_id = ?", scope.tenantID), scope).
		Select("currency, COALESCE(SUM(amount_minor),0) AS amount_minor, COUNT(*) AS contracts").Group("currency").Order("currency").Scan(&currencyRows).Error; err != nil {
		return domain.ContractSnapshot{}, false, err
	}
	currencyTotals := make([]domain.ContractCurrencyTotal, 0, len(currencyRows))
	for _, item := range currencyRows {
		currencyTotals = append(currencyTotals, domain.ContractCurrencyTotal{Currency: item.Currency, AmountMinor: item.AmountMinor, Contracts: item.Contracts})
	}
	// 组织/团队范围读的是异步同步的 dim_contract：源更新时间、同步水位和
	// 本次计算时间分别来自 dim_contract.updated_at、sync_source 和 time.Now()。
	// 同步失败/停用/超时只标记陈旧，不隐藏已同步数据。
	freshness := repository.scopedFreshness(ctx, scope.tenantID, "contract_management", application.ParseDashboardTimestamp(stringValue(row.SourceUpdatedAt)))
	snapshotAt := freshness.SourceUpdatedAt
	if snapshotAt == "" {
		snapshotAt = freshness.SyncedAt
	}
	return domain.ContractSnapshot{
		Available: row.TotalContracts > 0, TenantID: scope.tenantID, SnapshotAt: snapshotAt, Freshness: freshness,
		TotalAmountMinor: row.TotalAmountMinor, CurrencyTotals: currencyTotals, TotalContracts: row.TotalContracts,
		ApprovalContracts: row.ApprovalContracts, ActiveContracts: row.ActiveContracts, ExpiredContracts: row.ExpiredContracts,
		DiscountBuckets: map[string]int64{}, ConversionAvailable: false,
		ConversionBasis:           "按组织/团队范围聚合；转化率需合同与商机同粒度事实后开放。",
		DiscountUnavailableReason: "按组织/团队范围尚无折扣分桶事实。",
	}, row.TotalContracts > 0, nil
}

// scopedProject reads the publication marker and its dimension rows in one RR
// snapshot, so a concurrent atomic publish cannot mix generations.
func (repository *GORMRepository) scopedProject(ctx context.Context, scope queryScope) (domain.ProjectSnapshot, bool, error) {
	result := domain.ProjectSnapshot{StatusCounts: map[string]int{}}
	if scope.mode != "TENANT" && (scope.mode != "ORG" && scope.mode != "TEAM" || len(scope.orgUnitIDs) == 0) {
		return result, false, nil
	}
	result.TenantID = scope.tenantID
	result.Freshness = application.BuildFreshness(application.FreshnessInput{}, time.Now().UTC())
	available := false
	err := repository.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var state aggregation.ProjectProjectionState
		err := tx.Where("tenant_id=? AND projection_code=?", scope.tenantID, aggregation.ProjectProjectionCode).Take(&state).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		// Legacy dimensions and an unfinished first run do not prove publication.
		if !projectPublished(state) {
			return nil
		}
		var rows []struct {
			Status                              string
			Count, Risk, InFlight, ServiceItems int
			SourceUpdatedAt                     *string
		}
		err = applyOrgScope(tx.Table("dim_project").Where("tenant_id=? AND snapshot_id=?", scope.tenantID, state.PublishedSnapshotID), scope, "owner_org_id").Select("status, COUNT(*) count, COALESCE(SUM(risk),0) risk, COALESCE(SUM(in_flight),0) in_flight, COALESCE(SUM(service_items),0) service_items, MAX(source_updated_at) source_updated_at").Group("status").Scan(&rows).Error
		if err != nil {
			return err
		}
		var sourceAt *time.Time
		for _, row := range rows {
			result.StatusCounts[row.Status] = row.Count
			result.ProjectCount += row.Count
			result.RiskProjects += row.Risk
			result.InFlightProjects += row.InFlight
			result.ServiceItems += row.ServiceItems
			if at := application.ParseDashboardTimestamp(stringValue(row.SourceUpdatedAt)); at != nil && (sourceAt == nil || at.After(*sourceAt)) {
				sourceAt = at
			}
		}
		enabled := true
		watermark, known, err := (&GORMRepository{db: tx}).syncWatermark(ctx, scope.tenantID, "project_management")
		if err != nil {
			return err
		}
		if known {
			enabled = watermark.Enabled
		}
		result.Freshness = application.BuildFreshness(application.FreshnessInput{SourceUpdatedAt: sourceAt, LastSuccessAt: state.LastSuccessAt, SyncStatus: state.Status, SyncSourceKnown: true, SyncEnabled: enabled}, time.Now().UTC())
		result.SnapshotAt = result.Freshness.SourceUpdatedAt
		result.Available = true
		available = true
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return result, available, err
}

// syncWatermark 是 sync_source 中对应子系统最近一次同步的水位。
// last_watermark 当前没有写入方，因此使用 last_run_at + last_status 作为既有水位。
type syncWatermark struct {
	LastRunAt  *string `gorm:"column:last_run_at"`
	LastStatus string  `gorm:"column:last_status"`
	Enabled    bool    `gorm:"column:enabled"`
}

// syncWatermark 返回指定租户/子系统最近一次登记的水位；没有记录时 known=false。
func (repository *GORMRepository) syncWatermark(ctx context.Context, tenantID, subsystemCode string) (syncWatermark, bool, error) {
	var row syncWatermark
	err := repository.db.WithContext(ctx).Table("sync_source").
		Select("last_run_at, last_status, enabled").
		Where("tenant_id = ? AND subsystem_code = ?", tenantID, subsystemCode).
		Order("updated_at DESC, id DESC").
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return syncWatermark{}, false, nil
	}
	if err != nil {
		return syncWatermark{}, false, err
	}
	return row, true, nil
}

// scopedFreshness 生成维表镜像分支的时间口径。水位读取失败只降级为“状态未知”，
// 仍返回已同步的旧数据（前端按陈旧标记展示），避免辅助水位让整个看板 500。
func (repository *GORMRepository) scopedFreshness(ctx context.Context, tenantID, subsystemCode string, sourceUpdatedAt *time.Time) domain.DataFreshness {
	watermark, known, err := repository.syncWatermark(ctx, tenantID, subsystemCode)
	if err != nil {
		slog.Warn("dashboard sync watermark unavailable", "subsystem", subsystemCode, "error", err)
		known = false
	}
	return application.BuildFreshness(application.FreshnessInput{
		SourceUpdatedAt: sourceUpdatedAt,
		LastSyncAt:      application.ParseDashboardTimestamp(stringValue(watermark.LastRunAt)),
		SyncStatus:      watermark.LastStatus,
		SyncSourceKnown: known,
		SyncEnabled:     watermark.Enabled,
	}, time.Now().UTC())
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// scopedTrend 按范围汇总普通合同趋势和项目创建趋势。
// 参数：ctx 为读取上下文，scope 为可信范围，months 为月份上限。
// 返回值：分币种趋势及数据库错误；历史/未知来源不计普通合同。
func (repository *GORMRepository) scopedTrend(ctx context.Context, scope queryScope, months int) ([]domain.TrendPoint, error) {
	if len(scope.orgUnitIDs) == 0 && (scope.mode != "SELF" || scope.userID == "") {
		return nil, nil
	}
	var contractFacts []struct {
		Period   string
		Currency string
		Amount   int64
		Count    int64
	}
	err := applyContractScope(repository.db.WithContext(ctx).Table("dim_contract").Where("origin_kind = 'NORMAL'").Where("tenant_id = ?", scope.tenantID), scope).
		Select("DATE_FORMAT(COALESCE(start_date, DATE(updated_at)), '%Y-%m') AS period, currency, COALESCE(SUM(amount_minor),0) AS amount, COUNT(*) AS count").
		Group("period,currency").Order("period DESC").Limit(months * 8).Scan(&contractFacts).Error
	if err != nil {
		return nil, err
	}
	contractRows := trendRowsByCurrency(contractFacts, months)
	projectByPeriod, err := repository.projectCreationTrend(ctx, scope)
	if err != nil {
		return nil, err
	}
	return mergeTrendPoints(contractRows, projectByPeriod, months), nil
}

func trendRowsByCurrency(facts []struct {
	Period   string
	Currency string
	Amount   int64
	Count    int64
}, months int) []domain.TrendPoint {
	byPeriod := map[string]*domain.TrendPoint{}
	order := []string{}
	for _, fact := range facts {
		point := byPeriod[fact.Period]
		if point == nil {
			point = &domain.TrendPoint{Period: fact.Period}
			byPeriod[fact.Period] = point
			order = append(order, fact.Period)
		}
		point.ContractAmountMinor += fact.Amount
		point.ContractCount += fact.Count
		point.CurrencyTotals = append(point.CurrencyTotals, domain.ContractCurrencyTotal{Currency: fact.Currency, AmountMinor: fact.Amount, Contracts: fact.Count})
	}
	rows := make([]domain.TrendPoint, 0, len(order))
	for _, period := range order {
		if len(rows) >= months {
			break
		}
		rows = append(rows, *byPeriod[period])
	}
	return rows
}

// mergeTrendPoints 在应用月份上限前，合并合同事实与每月最新
// 项目快照，使仅有项目的月份仍可见。
func mergeTrendPoints(contractRows []domain.TrendPoint, projectByPeriod map[string]int, months int) []domain.TrendPoint {
	byPeriod := make(map[string]domain.TrendPoint, len(contractRows)+len(projectByPeriod))
	for _, row := range contractRows {
		row.ProjectCount = int64(projectByPeriod[row.Period])
		byPeriod[row.Period] = row
	}
	for period, projectCount := range projectByPeriod {
		if _, exists := byPeriod[period]; exists {
			continue
		}
		byPeriod[period] = domain.TrendPoint{Period: period, ProjectCount: int64(projectCount)}
	}
	periods := make([]string, 0, len(byPeriod))
	for period := range byPeriod {
		periods = append(periods, period)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(periods)))
	if len(periods) > months {
		periods = periods[:months]
	}
	rows := make([]domain.TrendPoint, 0, len(periods))
	for _, period := range periods {
		rows = append(rows, byPeriod[period])
	}
	return rows
}

// projectCreationTrend uses source creation months only after a complete publish.
func (repository *GORMRepository) projectCreationTrend(ctx context.Context, scope queryScope) (map[string]int, error) {
	counts := map[string]int{}
	if scope.mode != "TENANT" && (scope.mode != "ORG" && scope.mode != "TEAM" || len(scope.orgUnitIDs) == 0) {
		return counts, nil
	}
	err := repository.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var state aggregation.ProjectProjectionState
		err := tx.Where("tenant_id=? AND projection_code=?", scope.tenantID, aggregation.ProjectProjectionCode).Take(&state).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if !projectPublished(state) {
			return nil
		}
		var rows []struct {
			Period string
			Count  int
		}
		expression := "DATE_FORMAT(created_at, '%Y-%m')"
		if tx.Dialector.Name() == "sqlite" {
			expression = "strftime('%Y-%m', created_at)"
		}
		err = applyOrgScope(tx.Table("dim_project").Where("tenant_id=? AND snapshot_id=?", scope.tenantID, state.PublishedSnapshotID), scope, "owner_org_id").Select(expression + " period,COUNT(*) count").Group("period").Scan(&rows).Error
		if err != nil {
			return err
		}
		for _, row := range rows {
			counts[row.Period] = row.Count
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return counts, err
}
func projectPublished(state aggregation.ProjectProjectionState) bool {
	return state.LastSuccessAt != nil && state.PublishedRunID != "" && state.PublishedSnapshotID != "" && state.PublishedSourceSnapshotAt != nil && len(state.PublishedChecksumSHA256) == 64
}
