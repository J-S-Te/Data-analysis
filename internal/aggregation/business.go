package aggregation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type businessRow struct {
	TenantID   string `gorm:"primaryKey"`
	Section    string `gorm:"primaryKey"`
	SnapshotAt time.Time
	Payload    string
}

func (businessRow) TableName() string { return "business_dashboard_snapshot" }

func (r *Runner) storeBusiness(ctx context.Context, section string, value domain.BusinessSnapshot) error {
	if err := r.checkLicense(ctx); err != nil {
		return err
	}
	value.Available, value.TenantID = true, r.tenantID
	now := time.Now().UTC()
	value.SnapshotAt = now.Format(time.RFC3339Nano)
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return r.aggregationWrite(ctx, func(tx *gorm.DB) error {
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
			snapshot, e = financeStatistics(ctx, tx, r.tenantID, time.Now().UTC())
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

func financeStatistics(ctx context.Context, db *gorm.DB, tenant string, now time.Time) (domain.BusinessSnapshot, error) {
	cutoff := now.AddDate(0, 0, -90).Format("2006-01-02")
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
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
	if err := db.WithContext(ctx).Table("settlement_receivable").Select("currency, ROUND(SUM(original_amount)*100) original_minor, ROUND(SUM(received_allocated_amount)*100) allocated_minor, ROUND(SUM(open_amount)*100) open_minor, ROUND(SUM(CASE WHEN due_date < ? THEN open_amount ELSE 0 END)*100) over90_minor", cutoff).Where("tenant_id = ? AND recognition_status = 'CONFIRMED'", tenant).Group("currency").Order("currency").Scan(&rows).Error; err != nil {
		return domain.BusinessSnapshot{}, err
	}
	var receipts []struct {
		Currency string
		Minor    int64
	}
	if err := db.WithContext(ctx).Table("settlement_receipt").Select("currency, ROUND(SUM(amount)*100) minor").Where("tenant_id = ? AND status IN ('AVAILABLE','PARTIALLY_ALLOCATED','FULLY_ALLOCATED') AND receipt_date >= ? AND receipt_date < ?", tenant, monthStart.Format("2006-01-02"), monthStart.AddDate(0, 1, 0).Format("2006-01-02")).Group("currency").Order("currency").Scan(&receipts).Error; err != nil {
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
	return domain.BusinessSnapshot{Metrics: map[string]any{"currencies": rows, "month": monthStart.Format("2006-01"), "as_of_date": now.Format("2006-01-02")}, MissingMetrics: []string{"recognized_income", "department_cost"}, Basis: "按币种分别计算，金额为分；回款率=已核销回款/原始应收；账龄按到期日超过90天的未清余额；本月已登记回款不是收入确认，使用UTC业务日期。"}, nil
}

// syncProject publishes the API projection and optional report independently.
// Report statistics never serve as evidence that a complete projection exists.
func (r *Runner) syncProject(ctx context.Context, dsn string) error {
	var projectionErr, reportErr error
	if r.ProjectProjection.URL != "" {
		projectionErr = r.syncProjectProjection(ctx)
	}
	if dsn != "" {
		reportErr = r.syncBusiness(ctx, "project_management", dsn)
	}
	if r.ProjectProjection.URL == "" && dsn == "" {
		return errors.New("project source is not configured")
	}
	return errors.Join(projectionErr, reportErr)
}
