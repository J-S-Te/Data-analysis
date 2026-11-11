package aggregation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	// 业务日时区依赖 IANA tzdata。聚合进程与看板 API 是两个独立二进制，
	// 内嵌一份可确保精简镜像缺少 /usr/share/zoneinfo 时 LoadLocation 仍可用，
	// 避免财务快照业务日静默回退 UTC（约 400KB 体积代价，可接受）。
	_ "time/tzdata"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// DefaultBusinessTimezone 是财务快照业务日的默认时区，与看板“今天/到期”
// 业务日（DASHBOARD_TIMEZONE）默认值一致，避免同一租户两处日界不同。
const DefaultBusinessTimezone = "Asia/Shanghai"

// LoadBusinessLocation 解析并校验财务快照业务日时区。
// 参数：name 为 IANA 时区名，空值（含纯空白）取默认值 Asia/Shanghai。
// 返回值：时区对象，或非法时区错误。非法值必须让进程启动失败（fail-closed），
// 不把 as_of_date/月前缀/90 天 cutoff 悄悄退回 UTC。
func LoadBusinessLocation(name string) (*time.Location, error) {
	if strings.TrimSpace(name) == "" {
		name = DefaultBusinessTimezone
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("DASHBOARD_TIMEZONE %q is not a valid IANA time zone", name)
	}
	return location, nil
}

// businessLocation 返回财务快照业务日时区；未注入时退回 UTC，
// 保持既有调用方（含测试）的日界语义不变。
func (r *Runner) businessLocation() *time.Location {
	if r.BusinessLocation == nil {
		return time.UTC
	}
	return r.BusinessLocation
}

// BusinessUTCOffset 把业务时区相对 UTC 的偏移格式化为 ±HH:MM（如 "+08:00"）。
// 参数：loc 为业务时区（nil 视为 UTC），at 为取偏移的时刻（DST 时区按该时刻判定）。
// 返回值：MySQL CONVERT_TZ 目标时区/SQLite 时间修饰符都接受的偏移字符串。
// 用固定偏移而不是命名时区是有意为之：MySQL 容器常未加载 tz 表，
// CONVERT_TZ(...,'UTC','Asia/Shanghai') 会静默返回 NULL；数字偏移无需 tz 表。
func BusinessUTCOffset(loc *time.Location, at time.Time) string {
	if loc == nil {
		loc = time.UTC
	}
	_, seconds := at.In(loc).Zone()
	sign := "+"
	if seconds < 0 {
		sign, seconds = "-", -seconds
	}
	return fmt.Sprintf("%s%02d:%02d", sign, seconds/3600, (seconds%3600)/60)
}

type businessRow struct {
	TenantID   string `gorm:"primaryKey"`
	Section    string `gorm:"primaryKey"`
	SnapshotAt time.Time
	Payload    string
}

func (businessRow) TableName() string { return "business_dashboard_snapshot" }

// storeBusiness 原子保存本租户来源快照，真实来源时间不被本地同步时间覆盖。
// 参数：ctx 为许可/租约上下文，section 为指标分区，value 为已验证的完整快照。
// 返回值：持久化或权限错误；拒绝同来源水位倒退并保留既有快照。
func (r *Runner) storeBusiness(ctx context.Context, section string, value domain.BusinessSnapshot) error {
	if err := r.checkLicense(ctx); err != nil {
		return err
	}
	value.Available, value.TenantID = true, r.tenantID
	if value.SourceCode == "" {
		value.SourceCode = businessSourceCode(section)
	}
	now := time.Now().UTC()
	value.SnapshotAt = now.Format(time.RFC3339Nano)
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return r.aggregationWrite(ctx, func(tx *gorm.DB) error {
		if err := verifyBusinessWatermark(tx, r.tenantID, section, value); err != nil {
			return err
		}
		return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "tenant_id"}, {Name: "section"}}, DoUpdates: clause.AssignmentColumns([]string{"snapshot_at", "payload"})}).Create(&businessRow{r.tenantID, section, now, string(encoded)}).Error
	})
}

// 使用现有只读源通道，凭据必须仅有 SELECT 权限；
// 此适配器及失败聚合均不能创建或修改上游业务行。
func (r *Runner) syncBusiness(ctx context.Context, code, dsn string) error {
	src, err := gorm.Open(mysql.Open(dsn), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		return errors.New("open business statistics source failed")
	}
	pool, err := src.DB()
	if err != nil {
		return err
	}
	defer closeSourceConnectionPool(pool, code)
	var snapshot domain.BusinessSnapshot
	section := "finance"
	err = src.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var e error
		if code == "project_management" {
			section = "report"
			snapshot, e = reportStatistics(ctx, tx, r.tenantID)
		} else {
			// 财务快照的“今天”按业务日时区换算；存储时间戳仍由 storeBusiness 用 UTC。
			snapshot, e = financeStatistics(ctx, tx, r.tenantID, time.Now().In(r.businessLocation()))
		}
		return e
	}, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return fmt.Errorf("aggregate %s statistics: %w", section, err)
	}
	return r.storeBusiness(ctx, section, snapshot)
}

func reportStatistics(ctx context.Context, db *gorm.DB, tenant string) (domain.BusinessSnapshot, error) {
	q := db.WithContext(ctx).Table("pm_report_revision").Where("tenant_id = ?", tenant)
	var rows []struct {
		Status         string
		ValidityStatus string
		Count          int64
	}
	if err := q.Select("status, validity_status, COUNT(*) count").Group("status, validity_status").Scan(&rows).Error; err != nil {
		return domain.BusinessSnapshot{}, err
	}
	counts := map[string]int64{}
	var active, void int64
	for _, row := range rows {
		if row.ValidityStatus == "ACTIVE" {
			counts[row.Status] += row.Count
			active += row.Count
		} else if row.ValidityStatus == "VOID" {
			void += row.Count
		}
	}
	cycles := db.WithContext(ctx).Table("pm_report_revision").Where("tenant_id = ? AND validity_status = 'ACTIVE' AND issued_at IS NOT NULL AND prepared_at IS NOT NULL AND issued_at >= prepared_at", tenant)
	var sampleCount int64
	if err := cycles.Count(&sampleCount).Error; err != nil {
		return domain.BusinessSnapshot{}, err
	}
	var median *float64
	if sampleCount > 0 {
		expression := "TIMESTAMPDIFF(SECOND, prepared_at, issued_at) / 86400.0"
		if db.Dialector.Name() == "sqlite" {
			expression = "julianday(issued_at) - julianday(prepared_at)"
		}
		var samples []struct{ Days float64 }
		limit := 1
		if sampleCount%2 == 0 {
			limit = 2
		}
		if err := cycles.Select(expression + " days").Order("days ASC, id ASC").Offset(int((sampleCount - 1) / 2)).Limit(limit).Scan(&samples).Error; err != nil {
			return domain.BusinessSnapshot{}, err
		}
		if len(samples) != limit {
			return domain.BusinessSnapshot{}, errors.New("report median source changed")
		}
		value := samples[0].Days
		if limit == 2 {
			value = (value + samples[1].Days) / 2
		}
		median = &value
	}
	return domain.BusinessSnapshot{Metrics: map[string]any{"active_reports": active, "void_reports": void, "status_counts": counts, "cycle_p50_days": median, "cycle_samples": sampleCount, "reviewed_reports": counts["REVIEWED"] + counts["APPROVED"] + counts["ISSUED"] + counts["ARCHIVED"]}, MissingMetrics: []string{"first_pass_rate", "open_complaints"}, Basis: "报告版本台账：有效版本按状态计数；周期为 prepared_at 至 issued_at，P50 只计时间完整且非负的已签发版本；作废版本单列，不伪造首次通过率和投诉。"}, nil
}

// financeDateBoundaries 依据业务日时区的“现在”计算财务快照的三个日期边界。
// 参数：now 为业务日时区下的时刻（调用方用 Runner.BusinessLocation 换算）。
// 返回值：as_of_date（2006-01-02）、90 天账龄截止日（2006-01-02）与业务时区当月首日；
// 只产出日期边界，不改变金额/账龄 SQL 语义，存储时间戳仍为 UTC。
func financeDateBoundaries(now time.Time) (asOfDate, cutoff string, monthStart time.Time) {
	asOfDate = now.Format("2006-01-02")
	cutoff = now.AddDate(0, 0, -90).Format("2006-01-02")
	// 本月首日按业务时区构造，不再固定 UTC：否则北京时间每月首日凌晨的回款
	// 会落到上一个月，与看板“本月”口径不一致。
	monthStart = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	return asOfDate, cutoff, monthStart
}

// financeStatistics 按币种汇总应收、账龄与本月回款。
// 参数：ctx 为查询上下文；db 为只读源库连接；tenant 为租户隔离边界；now 为业务日
// 时区下的“现在”（调用方用 Runner.BusinessLocation 换算）。now 只决定 as_of_date、
// 90 天 cutoff 与本月前缀三个日期边界，金额/账龄 SQL 语义不变，存储仍为 UTC。
// 返回值：快照或查询错误。
func financeStatistics(ctx context.Context, db *gorm.DB, tenant string, now time.Time) (domain.BusinessSnapshot, error) {
	asOfDate, cutoff, monthStart := financeDateBoundaries(now)
	type currencyTotals struct {
		Currency           string   `json:"currency"`
		OriginalMinor      int64    `json:"original_minor"`
		AllocatedMinor     int64    `json:"allocated_minor"`
		OpenMinor          int64    `json:"open_minor"`
		Over90Minor        int64    `json:"over_90_minor"`
		MonthReceiptsMinor int64    `json:"month_receipts_minor"`
		CollectionRate     *float64 `json:"collection_rate"`
	}
	var rows []currencyTotals
	if err := db.WithContext(ctx).Table("settlement_receivable AS r").
		Joins("JOIN settlement_contract_snapshot cs ON cs.id=r.contract_snapshot_id AND cs.tenant_id=r.tenant_id").
		Select(`r.currency, ROUND(SUM(r.original_amount)*100) original_minor,
            ROUND(SUM(r.received_allocated_amount)*100) allocated_minor,
            ROUND(SUM(r.open_amount)*100) open_minor,
            ROUND(SUM(CASE WHEN r.due_date < ? THEN r.open_amount ELSE 0 END)*100) over90_minor`, cutoff).
		Where("r.tenant_id=? AND r.recognition_status='CONFIRMED' AND cs.migration_state='RELEASED'", tenant).
		Group("r.currency").Order("r.currency").Scan(&rows).Error; err != nil {
		return domain.BusinessSnapshot{}, err
	}
	var receipts []struct {
		Currency string
		Minor    int64
	}
	if err := db.WithContext(ctx).Table("settlement_receipt AS rc").
		Select("rc.currency, ROUND(SUM(rc.amount)*100) minor").
		Where(`rc.tenant_id=? AND rc.status IN ('AVAILABLE','PARTIALLY_ALLOCATED','FULLY_ALLOCATED')
            AND rc.receipt_date >= ? AND rc.receipt_date < ?
            AND NOT EXISTS (SELECT 1 FROM settlement_import_fact f
                JOIN settlement_contract_snapshot cs ON cs.id=f.contract_snapshot_id AND cs.tenant_id=f.tenant_id
                WHERE f.tenant_id=rc.tenant_id AND f.target_business_id=rc.id AND f.fact_type='RECEIPT'
                    AND cs.migration_state <> 'RELEASED')`, tenant,
			monthStart.Format("2006-01-02"), monthStart.AddDate(0, 1, 0).Format("2006-01-02")).
		Group("rc.currency").Order("rc.currency").Scan(&receipts).Error; err != nil {
		return domain.BusinessSnapshot{}, err
	}
	byCurrency := map[string]int{}
	for i := range rows {
		byCurrency[rows[i].Currency] = i
		if rows[i].OriginalMinor > 0 {
			rate := 100 * float64(rows[i].AllocatedMinor) / float64(rows[i].OriginalMinor)
			rows[i].CollectionRate = &rate
		}
	}
	for _, receipt := range receipts {
		i, ok := byCurrency[receipt.Currency]
		if !ok {
			i = len(rows)
			rows = append(rows, currencyTotals{Currency: receipt.Currency})
			byCurrency[receipt.Currency] = i
		}
		rows[i].MonthReceiptsMinor = receipt.Minor
	}
	if rows == nil {
		rows = []currencyTotals{}
	}
	return domain.BusinessSnapshot{Metrics: map[string]any{"currencies": rows, "month": monthStart.Format("2006-01"), "as_of_date": asOfDate}, MissingMetrics: []string{"recognized_income", "department_cost"}, Basis: "按币种分别计算，金额为分；只计RELEASED合同的CONFIRMED应收及非HOLD有效回款；回款率=已核销回款/原始应收；账龄按到期日超过90天的未清余额；本月已登记回款不是收入确认；as_of_date、90天账龄截止日与本月前缀按业务日时区（DASHBOARD_TIMEZONE，默认 Asia/Shanghai）计算，存储时间戳仍为 UTC。"}, nil
}

// syncProject 分别同步完整项目投影和报告指标，报告不作为投影完整性的凭据。
// 参数：ctx 为许可/租约上下文，dsn 为可选历史只读通道。
// 返回值：各已配置通道错误；API配置后失败不切换到其他来源伪造成功。
func (r *Runner) syncProject(ctx context.Context, dsn string) error {
	var projectionErr, reportErr error
	if r.ProjectProjection.URL != "" {
		projectionErr = r.syncProjectProjection(ctx)
	}
	apiConfigured := r.ProjectAPI != nil && r.ProjectAPI.options.ProjectReportURL != ""
	if apiConfigured {
		reportErr = r.syncBusinessHTTP(ctx, "report", r.ProjectAPI.options.ProjectReportURL,
			r.ProjectAPI.options.ProjectCredential)
	} else if dsn != "" {
		reportErr = r.syncBusiness(ctx, "project_management", dsn)
	}
	if r.ProjectProjection.URL == "" && dsn == "" && !apiConfigured {
		return errors.New("project source is not configured")
	}
	return errors.Join(projectionErr, reportErr)
}
