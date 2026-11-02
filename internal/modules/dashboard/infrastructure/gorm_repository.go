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
	// 业务日时区依赖 IANA tzdata。内嵌一份可确保精简容器/构建镜像缺少
	// /usr/share/zoneinfo 时 LoadLocation 仍可用，避免业务日静默回退 UTC；
	// 代价是二进制增大 zoneinfo 体积（约 400KB），可接受。
	_ "time/tzdata"

	"gorm.io/gorm"

	"github.com/unified-identity-auth-platform/data-analysis/internal/aggregation"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
)

// DashboardTimezone 是看板“今天/到期”业务日的默认时区。
// 存储、同步与其它时间戳仍全部使用 UTC，只有业务日边界按此换算，
// 避免 UTC 00:00（北京时间 08:00，前一晚刚跨日）让到期状态与实际作息不符。
const DashboardTimezone = "Asia/Shanghai"

// GORMRepository 从 MySQL 视图读取物化看板快照。
type GORMRepository struct {
	db *gorm.DB
	// location 是“今天/到期”业务日所在时区；为零值时按 UTC 处理。
	location *time.Location
	// approvalSource 是“审批中”口径；零值按 status（源状态 pending 近似）处理，
	// 保证既有调用方行为不变。
	approvalSource ApprovalSource
}

// NewGORMRepositoryWithLocation 构造业务日时区明确的看板持久化适配器。
// 参数：db 为聚合库连接，location 为已校验的业务日时区（nil 视为 UTC）。
// 返回值：看板仓储（审批中口径默认 status）。
func NewGORMRepositoryWithLocation(db *gorm.DB, location *time.Location) *GORMRepository {
	if location == nil {
		location = time.UTC
	}
	return &GORMRepository{db: db, location: location}
}

// NewGORMRepositoryWithApprovalSource 构造审批中口径明确的看板持久化适配器。
// 参数：db 为聚合库连接，location 为业务日时区（nil 视为 UTC），source 为已解析口径。
// 返回值：看板仓储；source 为零值/未知值时按 status 处理（调用方启动期已用
// ParseApprovalSource 拒绝非法值，这里只做防御性归一）。
func NewGORMRepositoryWithApprovalSource(db *gorm.DB, location *time.Location, source ApprovalSource) *GORMRepository {
	repository := NewGORMRepositoryWithLocation(db, location)
	repository.approvalSource = normalizeApprovalSource(source)
	return repository
}

// businessLocation 返回业务日时区；零值按 UTC 处理。
// 参数：无。
// 返回值：非空时区。
func (repository *GORMRepository) businessLocation() *time.Location {
	if repository.location == nil {
		return time.UTC
	}
	return repository.location
}

// businessOffset 返回业务时区相对 UTC 的偏移（如 "+08:00"），供 CONVERT_TZ 绑定。
// 参数：now 为取偏移的时刻（DST 时区按该时刻判定）。
// 返回值：MySQL CONVERT_TZ 目标时区字符串。
// 存储的 snapshot_at/created_at/updated_at 都是 UTC 墙钟 DATETIME，取月前缀前
// 必须先换算到业务时区，否则北京时间每月首日 00:00-08:00 的数据会落到上一月。
func (repository *GORMRepository) businessOffset(now time.Time) string {
	return aggregation.BusinessUTCOffset(repository.businessLocation(), now)
}

// businessMonthWindowStart 返回"近 months 个月"趋势窗口下界。
// 参数：now 为当前时刻，months 为月份数（不足 1 时按 1）。
// 返回值：业务当月月初再回退 months-1 个月的 UTC 墙钟 DATETIME 字符串。
// 不再用 UTC_TIMESTAMP() 做窗口下界：那是 UTC 的"现在"，与业务月边界相差一个
// 偏移量，会把业务当月首日凌晨的数据切到窗口外。用字符串绑定是为了不受 DSN loc
// 影响（值已是 UTC 墙钟，可直接与 snapshot_at 比较）。
func (repository *GORMRepository) businessMonthWindowStart(now time.Time, months int) string {
	if months < 1 {
		months = 1
	}
	location := repository.businessLocation()
	local := now.In(location)
	start := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, location).AddDate(0, -(months - 1), 0)
	return start.UTC().Format("2006-01-02 15:04:05")
}

// businessDate 返回业务日时区下的当日日期（YYYY-MM-DD）。
// 只用于“今天/到期”派生边界，不影响任何存储或时间戳口径。
func (repository *GORMRepository) businessDate(now time.Time) string {
	return now.In(repository.businessLocation()).Format("2006-01-02")
}

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

// projectTrendQuery 按月取每租户最新项目快照；月前缀按业务时区归集。
// 占位符顺序：业务时区偏移（月前缀）、业务时区偏移（窗口分区）、租户、
// 业务月窗口下界、LIMIT。偏移以参数传入，不硬编码 +08:00。
const projectTrendQuery = `SELECT period, project_count FROM (
	SELECT DATE_FORMAT(CONVERT_TZ(snapshot_at, '+00:00', ?), '%Y-%m') AS period, project_count,
	       ROW_NUMBER() OVER (PARTITION BY DATE_FORMAT(CONVERT_TZ(snapshot_at, '+00:00', ?), '%Y-%m') ORDER BY snapshot_at DESC, id DESC) AS rn
	FROM api_project_dashboard
	WHERE tenant_id = ? AND snapshot_at >= ?
) t WHERE rn = 1 ORDER BY period DESC LIMIT ?`

// contractSigningPeriodExpr 返回合同签约月的分组表达式（含业务时区偏移占位符）。
// 参数：dialect 为数据库方言名（sqlite 走 strftime 时间修饰符）。
// 返回值：带一个绑定占位符的月前缀表达式。
// start_date 是 DATE 无时区，回退分支 updated_at 是 UTC 墙钟，必须先换算到业务时区。
func contractSigningPeriodExpr(dialect string) string {
	if dialect == "sqlite" {
		return "strftime('%Y-%m', COALESCE(start_date, datetime(updated_at, ?)))"
	}
	return "DATE_FORMAT(COALESCE(start_date, DATE(CONVERT_TZ(updated_at, '+00:00', ?))), '%Y-%m')"
}

// projectCreationPeriodExpr 返回项目创建月的分组表达式（含业务时区偏移占位符）。
// 参数：dialect 为数据库方言名。
// 返回值：带一个绑定占位符的月前缀表达式。
// created_at 是 UTC 墙钟，按业务时区取创建月，避免月初数据落到上一月。
func projectCreationPeriodExpr(dialect string) string {
	if dialect == "sqlite" {
		return "strftime('%Y-%m', created_at, ?)"
	}
	return "DATE_FORMAT(CONVERT_TZ(created_at, '+00:00', ?), '%Y-%m')"
}

// projectTrendArgs 返回 projectTrendQuery 的绑定参数，顺序与占位符一一对应：
// 业务时区偏移（月前缀）、业务时区偏移（窗口分区）、租户、业务月窗口下界、LIMIT。
// 参数：now 为当前时刻，tenantID 为租户，months 为月份上限。
// 返回值：与 projectTrendQuery 占位符顺序一致的参数切片。
func (repository *GORMRepository) projectTrendArgs(now time.Time, tenantID string, months int) []any {
	offset := repository.businessOffset(now)
	return []any{offset, offset, tenantID, repository.businessMonthWindowStart(now, months), months}
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
	// 业务月：snapshot_at 是 UTC 墙钟，先 CONVERT_TZ 到业务时区再取月前缀；
	// 偏移以绑定参数传入（不硬编码 +08:00）。窗口下界同样取业务月边界。
	now := time.Now()
	if err := repository.db.WithContext(ctx).Raw(projectTrendQuery, repository.projectTrendArgs(now, tenantID, months)...).Scan(&projects).Error; err != nil {
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

// applyOrgScope 将可信范围应用到“归属组织单元”列（当前均为 owner_org_id）。
// 参数：query 为已限定租户的查询，scope 为可信主体范围，column 为归属组织单元列。
// 返回值：范围内查询；未知模式或空集合返回恒空条件。
//
// ORG 与 TEAM 的 ID 都来自平台 ORG 类型的数据范围绑定（见 internal/oidc/scope.go），
// 属于同一组织单元命名空间，因此都映射到 owner_org_id。dim_project.team 是团队名称
// 快照而不是授权键，不能作为过滤列；若未来平台引入独立团队实体，必须在此显式拒绝
// TEAM 模式，而不是猜测列映射。
func applyOrgScope(query *gorm.DB, scope queryScope, column string) *gorm.DB {
	switch scope.mode {
	case "TENANT":
		return query
	case "ORG", "TEAM":
		if !scope.hasScopedIDs() {
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
	if !scope.contractRowsVisible() {
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
	// 状态口径必须是合同源词表 + 到期派生，不能使用 APPROVING/EXPIRED/EFFECTIVE/SIGNED
	// 这类派生词：dim_contract.status 存的是 contract_management 生命周期状态
	// (draft/pending/approved/active/in_progress/pending_pay/completed/terminated/archived)。
	// 派生规则与合同系统全租户看板一致：
	// 审批中=审批实例在途（DASHBOARD_APPROVAL_SOURCE=instances 时用
	// dim_contract_approval 的 EXISTS(status='running')，含变更审批；默认 status
	// 则沿用源状态 pending 近似）；履行中=active/in_progress/pending_pay 且未到期，
	// 外加终态 archived（源系统亦计入有效合同且从不算到期）；已到期=active 族且 end_date 早于业务日。
	// 业务日期用绑定参数（业务时区当日，默认 Asia/Shanghai），等价于源系统的本地
	// CURDATE() 而不再用 UTC 当日，避免北京时间 00:00-08:00 期间到期口径滞后一天。
	//
	// instances 口径是升级门禁：审批投影通道从未成功时必须拒绝（fail-closed），
	// 否则会把“近似 >0”变成恒 0 的静默回归。
	if normalizeApprovalSource(repository.approvalSource) == ApprovalSourceInstances {
		ready, err := ApprovalInstancesReady(ctx, repository.db, scope.tenantID)
		if err != nil {
			return domain.ContractSnapshot{}, false, fmt.Errorf("approval instances source unavailable: %w", err)
		}
		if !ready {
			return domain.ContractSnapshot{}, false, ErrApprovalSourceNotReady
		}
	}
	today := repository.businessDate(time.Now())
	scoped := applyContractScope(repository.db.WithContext(ctx).Table("dim_contract").Where("origin_kind = 'NORMAL'").Where("tenant_id = ?", scope.tenantID), scope)
	err := scoped.Select(`COALESCE(SUM(amount_minor),0) AS total_amount_minor, COUNT(*) AS total_contracts,
			`+repository.approvalCountExpr()+`,
			COALESCE(SUM(CASE WHEN status='archived' OR (status IN ('active','in_progress','pending_pay') AND (end_date IS NULL OR end_date >= ?)) THEN 1 ELSE 0 END),0) AS active_contracts,
			COALESCE(SUM(CASE WHEN status IN ('active','in_progress','pending_pay') AND end_date IS NOT NULL AND end_date < ? THEN 1 ELSE 0 END),0) AS expired_contracts,
			MAX(updated_at) AS source_updated_at`, today, today).Scan(&row).Error
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
	if !scope.projectRowsVisible() {
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
		watermark, known, err := (&GORMRepository{db: tx, location: repository.location}).syncWatermark(ctx, scope.tenantID, "project_management")
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
	if !scope.contractRowsVisible() {
		return nil, nil
	}
	var contractFacts []struct {
		Period   string
		Currency string
		Amount   int64
		Count    int64
	}
	// start_date 是 DATE（无时区），但回退分支 DATE(updated_at) 是 UTC 日，
	// 必须先换算到业务时区再取月前缀，否则签约月会落在上一个月。
	err := applyContractScope(repository.db.WithContext(ctx).Table("dim_contract").Where("origin_kind = 'NORMAL'").Where("tenant_id = ?", scope.tenantID), scope).
		Select(contractSigningPeriodExpr(repository.db.Dialector.Name())+" AS period, currency, COALESCE(SUM(amount_minor),0) AS amount, COUNT(*) AS count", repository.businessOffset(time.Now())).
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
	if !scope.projectRowsVisible() {
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
		// created_at 是 UTC 墙钟，按业务时区取创建月；偏移以绑定参数传入。
		expression := projectCreationPeriodExpr(tx.Dialector.Name())
		err = applyOrgScope(tx.Table("dim_project").Where("tenant_id=? AND snapshot_id=?", scope.tenantID, state.PublishedSnapshotID), scope, "owner_org_id").Select(expression+" period,COUNT(*) count", repository.businessOffset(time.Now())).Group("period").Scan(&rows).Error
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
