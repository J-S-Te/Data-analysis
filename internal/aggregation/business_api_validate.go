package aggregation

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
)

const maxDashboardInteger = int64(9007199254740991)

// validateBusinessSnapshot 验证来源身份、完整水位及分区指标，拒绝缺数伪零。
// 参数：value 为已保留数字精度的来源快照，section/tenant/now 为可信预期。
// 返回值：契约有效时nil；错误仅包含字段标识，不包含上游原始内容。
func validateBusinessSnapshot(value domain.BusinessSnapshot, section, tenant string, now time.Time) error {
	if !value.Available || value.TenantID != tenant || value.Metrics == nil ||
		value.MissingMetrics == nil || value.Basis == "" {
		return errors.New("incomplete business statistics snapshot or tenant mismatch")
	}
	stamp, err := time.Parse(time.RFC3339Nano, value.SnapshotAt)
	if err != nil || stamp.After(now.Add(time.Minute)) {
		return errors.New("invalid business statistics source watermark")
	}
	if value.SourceUpdatedAt != "" {
		updated, err := time.Parse(time.RFC3339Nano, value.SourceUpdatedAt)
		if err != nil || updated.After(stamp.Add(time.Minute)) {
			return errors.New("invalid business statistics source update time")
		}
	}
	switch section {
	case "report":
		if value.SourceCode != "" && value.SourceCode != "project_management" {
			return errors.New("report source mismatch")
		}
		return validateReportMetrics(value.Metrics)
	case "finance":
		if value.SourceCode != "" && value.SourceCode != "settlement" {
			return errors.New("finance source mismatch")
		}
		return validateFinanceMetrics(value.Metrics)
	default:
		return errors.New("unsupported business statistics section")
	}
}

// businessInteger 读取非负安全整数，避免float转换或超过浏览器精确整数范围。
// 参数：values 为指标字典，key 为必须存在的整数键。
// 返回值：整数或契约错误；缺失/null/小数/溢出均拒绝。
func businessInteger(values map[string]any, key string) (int64, error) {
	number, ok := values[key].(json.Number)
	if !ok {
		return 0, fmt.Errorf("business statistics integer %s is missing", key)
	}
	value, err := number.Int64()
	if err != nil || value < 0 || value > maxDashboardInteger {
		return 0, fmt.Errorf("invalid business statistics integer %s", key)
	}
	return value, nil
}

// validateReportMetrics 校验报告状态合计、周期样本及nullable中位数。
// 参数：metrics 为报告来源指标。
// 返回值：指标一致时nil，缺失或矛盾时返回错误。
func validateReportMetrics(metrics map[string]any) error {
	counts := map[string]int64{}
	for _, key := range []string{"active_reports", "void_reports", "cycle_samples", "reviewed_reports"} {
		value, err := businessInteger(metrics, key)
		if err != nil {
			return err
		}
		counts[key] = value
	}
	states, ok := metrics["status_counts"].(map[string]any)
	if !ok {
		return errors.New("report status counts are missing")
	}
	var total int64
	for state := range states {
		value, err := businessInteger(states, state)
		if err != nil || total > maxDashboardInteger-value {
			return errors.New("invalid report status counts")
		}
		total += value
	}
	if total != counts["active_reports"] || counts["cycle_samples"] > total || counts["reviewed_reports"] > total {
		return errors.New("report totals are inconsistent")
	}
	cycle, exists := metrics["cycle_p50_days"]
	if !exists || counts["cycle_samples"] == 0 && cycle != nil {
		return errors.New("report cycle sample is inconsistent")
	}
	if counts["cycle_samples"] > 0 {
		number, ok := cycle.(json.Number)
		if !ok {
			return errors.New("report cycle median is missing")
		}
		days, err := number.Float64()
		if err != nil || math.IsNaN(days) || math.IsInf(days, 0) || days < 0 {
			return errors.New("invalid report cycle median")
		}
	}
	return nil
}

// validateFinanceMetrics 校验分币种余额、日期、有效回款和零分母语义。
// 参数：metrics 为财务来源指标。
// 返回值：无跨币种混算且各项完整一致时nil，否则错误。
func validateFinanceMetrics(metrics map[string]any) error {
	day, dayOK := metrics["as_of_date"].(string)
	month, monthOK := metrics["month"].(string)
	date, err := time.Parse("2006-01-02", day)
	if !dayOK || !monthOK || err != nil || date.Format("2006-01") != month {
		return errors.New("invalid finance business date")
	}
	rows, ok := metrics["currencies"].([]any)
	if !ok {
		return errors.New("finance currency facts are missing")
	}
	seen := map[string]bool{}
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			return errors.New("invalid finance currency row")
		}
		currency, ok := row["currency"].(string)
		if !ok || len(currency) != 3 || seen[currency] {
			return errors.New("invalid or duplicate finance currency")
		}
		for _, code := range currency {
			if code < 'A' || code > 'Z' {
				return errors.New("invalid finance currency code")
			}
		}
		seen[currency] = true
		if err := validateFinanceCurrency(row); err != nil {
			return err
		}
	}
	return nil
}

// validateFinanceCurrency 校验单币种净额与回款率，不将核销差额冒充回款。
// 参数：row 为单币种的完整金额字段。
// 返回值：余额和可空比率合法时nil，否则错误。
func validateFinanceCurrency(row map[string]any) error {
	values := map[string]int64{}
	for _, key := range []string{"original_minor", "allocated_minor", "open_minor", "over_90_minor", "month_receipts_minor"} {
		value, err := businessInteger(row, key)
		if err != nil {
			return err
		}
		values[key] = value
	}
	original, allocated := values["original_minor"], values["allocated_minor"]
	if allocated > original || values["open_minor"] > original-allocated || values["over_90_minor"] > values["open_minor"] {
		return errors.New("finance balances are inconsistent")
	}
	rate, exists := row["collection_rate"]
	if !exists || original == 0 && rate != nil {
		return errors.New("finance denominator is inconsistent")
	}
	if original == 0 {
		return nil
	}
	number, ok := rate.(json.Number)
	if !ok {
		return errors.New("finance collection rate is missing")
	}
	actual, err := number.Float64()
	expected := 100 * float64(allocated) / float64(original)
	if err != nil || math.IsNaN(actual) || math.IsInf(actual, 0) || math.Abs(actual-expected) > 0.000001 {
		return errors.New("finance collection rate is inconsistent")
	}
	return nil
}
