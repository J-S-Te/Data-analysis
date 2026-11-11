package metricfacts

import (
	"errors"
	"sort"

	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	sharedscope "github.com/unified-identity-auth-platform/data-analysis/internal/shared/scope"
)

// SourceFor 将看板分区映射为唯一权威指标源。
// 参数：section为已允许的看板分区。
// 返回值：来源编码；未知分区返回空串。
func SourceFor(section string) string {
	switch section {
	case "contract":
		return "contract_management"
	case "project", "report":
		return "project_management"
	case "finance":
		return "settlement"
	}
	return ""
}

// validateScope 拒绝缺失可信范围而按租户全量统计。
// 参数：scope为在线授权主体提供的当前范围。
// 返回值：已明确范围时nil，否则拒绝。
func validateScope(scope Scope) error {
	switch scope.Mode {
	case "TENANT":
		return nil
	case "SELF":
		if scope.UserID != "" {
			return nil
		}
	case "ORG", "ORG_SUBTREE", "TEAM":
		if ids, err := scopeOrganizations(scope); err == nil && len(ids) > 0 {
			return nil
		}
	}
	return ErrScope
}

// scopeOrganizations 复用既有组织及团队命名空间和授权并集规则。
// 参数：scope为可信在线主体的数据范围。
// 返回值：组织单元集合；命名空间歧义时返回错误且不返回任何ID。
func scopeOrganizations(scope Scope) ([]string, error) {
	mode := scope.Mode
	if mode == "ORG_SUBTREE" {
		mode = "ORG"
	}
	return sharedscope.OwnerOrgUnitIDs(mode, auth.Principal{
		OrganizationIDs: scope.OrganizationIDs, TeamIDs: scope.TeamIDs,
	})
}

// rowVisible 在任何汇总之前按冻结的真实组织或人员维度过滤。
// 参数：row为最小事实，scope为已校验当前范围。
// 返回值：事实属于授权范围时true，未知归属不授予窄范围访问。
func rowVisible(row Row, scope Scope) bool {
	switch scope.Mode {
	case "TENANT":
		return true
	case "SELF":
		return row.IdentityID != "" && row.IdentityID == scope.UserID
	case "ORG", "ORG_SUBTREE":
		return contains(scope.OrganizationIDs, row.OrganizationID)
	case "TEAM":
		return contains(scope.TeamIDs, row.OrganizationID)
	}
	return false
}

// contains 判断非空冻结标识是否在可信授权集合中。
// 参数：values为授权ID，value为来源维度。
// 返回值：精确匹配时true。
func contains(values []string, value string) bool {
	if value == "" {
		return false
	}
	for _, allowed := range values {
		if allowed == value {
			return true
		}
	}
	return false
}

// checkedAdd 在精确整数范围内累加金额或分钟。
// 参数：target为当前总量，value为本次有符号贡献。
// 返回值：越界时错误且不改变原总量。
func checkedAdd(target *int64, value int64) error {
	if value > MaxInteger || value < -MaxInteger ||
		value > 0 && *target > MaxInteger-value || value < 0 && *target < -MaxInteger-value {
		return errors.New("指标累计超过可精确展示的范围")
	}
	*target += value
	return nil
}

// percentage 保留无分母与真实零比例的区别。
// 参数：numerator为确认分子，denominator为明确分母。
// 返回值：百分比；分母为零时nil。
func percentage(numerator, denominator int64) *float64 {
	if denominator == 0 {
		return nil
	}
	value := 100 * float64(numerator) / float64(denominator)
	return &value
}

// Summarize 对完整合法来源先过滤范围和期间再计算安全统计。
// 参数：bundle为已验证快照，section/period为查询，scope为可信在线范围。
// 返回值：不含原始事实行的统计；范围、分区或累计溢出时错误。
func Summarize(bundle Bundle, section, period string, scope Scope) (Summary, error) {
	if err := validateScope(scope); err != nil {
		return Summary{}, err
	}
	if scope.Mode == "ORG" || scope.Mode == "ORG_SUBTREE" || scope.Mode == "TEAM" {
		ids, err := scopeOrganizations(scope)
		if err != nil {
			return Summary{}, ErrScope
		}
		scope.Mode, scope.OrganizationIDs = "ORG", ids
	}
	if SourceFor(section) == "" || bundle.SourceCode != SourceFor(section) ||
		period != "" && !ValidPeriod(period) {
		return Summary{}, ErrInvalid
	}
	out := Summary{Available: true, Section: section, Period: period,
		Basis: bundle.Basis, SourceCode: bundle.SourceCode, SnapshotAt: bundle.SnapshotAt,
		SourceUpdatedAt: bundle.SourceUpdatedAt, CollectionStart: bundle.CollectionStartedAt,
		DiscountBuckets: map[string]int64{}, ReturnReasons: map[string]int64{},
		Currencies: []CurrencyTotal{}, Costs: []CostDimension{}}
	currencies := map[string]*CurrencyTotal{}
	dimensions := map[string]*CostDimension{}
	returned := map[string]bool{}
	for _, row := range bundle.Rows {
		if !rowVisible(row, scope) {
			continue
		}
		if period != "" && row.Period != period {
			if section == "contract" && row.Kind == "discount" && row.Period == "" {
				out.Unknown++
			}
			currentReportCoverage := section == "report" && row.Kind == "report_review" &&
				row.Period == "" && (row.Status == "UNKNOWN" || row.Status == "PENDING")
			if currentReportCoverage {
				appendReview(&out, row, returned)
			}
			continue
		}
		if err := appendSummary(&out, row, currencies, dimensions, returned); err != nil {
			return Summary{}, err
		}
	}
	out.FirstPassRate = percentage(out.FirstPassed, out.FirstReviewed)
	out.Person.Rate = percentage(out.Person.ActualMinutes, out.Person.CapacityMinutes)
	out.Equipment.Rate = percentage(out.Equipment.ActualMinutes, out.Equipment.CapacityMinutes)
	out.ReturnedReports = int64(len(returned))
	for _, row := range currencies {
		out.Currencies = append(out.Currencies, *row)
	}
	for _, row := range dimensions {
		out.Costs = append(out.Costs, *row)
	}
	sort.Slice(out.Currencies, func(i, j int) bool { return out.Currencies[i].Currency < out.Currencies[j].Currency })
	sort.Slice(out.Costs, func(i, j int) bool {
		a, b := out.Costs[i], out.Costs[j]
		return a.Dimension+"\x00"+a.ID+"\x00"+a.Currency < b.Dimension+"\x00"+b.ID+"\x00"+b.Currency
	})
	return out, nil
}

// appendSummary 只累加属于所选分区的事实，避免现金或待审混入确认额。
// 参数：out为统计，row为范围内事实，其余参数为本次查询局部去重索引。
// 返回值：累计安全时nil，金额或分钟越界时错误。
func appendSummary(out *Summary, row Row, currencies map[string]*CurrencyTotal,
	dimensions map[string]*CostDimension, returned map[string]bool) error {
	switch out.Section {
	case "contract":
		if row.Kind == "discount" {
			if row.Status == "KNOWN" {
				out.DiscountKnown++
				out.DiscountBuckets[row.Bucket]++
			} else {
				out.DiscountUnknown++
			}
		}
	case "report":
		appendReview(out, row, returned)
	case "project":
		if row.Kind == "person_utilization" {
			return appendUtilization(&out.Person, row)
		}
		if row.Kind == "equipment_utilization" {
			return appendUtilization(&out.Equipment, row)
		}
	case "finance":
		return appendFinance(out, row, currencies, dimensions)
	}
	return nil
}

// appendReview 按真实首轮终决计算分母并按逻辑主体去重退回数量。
// 参数：out为报告统计，row为审核事实，returned为主体去重集合。
// 返回值：无。
func appendReview(out *Summary, row Row, returned map[string]bool) {
	if row.Kind == "report_review" {
		switch row.Status {
		case "PASSED":
			out.FirstPassed++
			out.FirstReviewed++
		case "RETURNED":
			out.FirstReviewed++
		case "PENDING":
			out.Pending++
		case "UNKNOWN":
			out.Unknown++
		}
	}
	if row.Kind == "report_return" {
		out.ReturnedDecisions++
		out.ReturnReasons[row.ReasonCode]++
		returned[row.SubjectID] = true
	}
}

// appendUtilization 汇总明确容量和已确认实际分钟，单列采集缺口。
// 参数：out为一种资源的累计值，row为该资源月份事实。
// 返回值：任意必需值缺失或累计溢出时错误。
func appendUtilization(out *Utilization, row Row) error {
	for _, item := range []struct {
		target *int64
		value  *int64
	}{
		{&out.ActualMinutes, row.ActualMinutes}, {&out.CapacityMinutes, row.CapacityMinutes},
		{&out.Pending, row.PendingCount}, {&out.Unknown, row.UnknownCount},
	} {
		if item.value == nil {
			return ErrInvalid
		}
		if err := checkedAdd(item.target, *item.value); err != nil {
			return err
		}
	}
	return nil
}

// appendFinance 按币种累加净确认和现金指标并保留授权范围内成本维度。
// 参数：out为财务统计，row为事实，其余参数为币种及维度累计索引。
// 返回值：金额缺失或溢出时错误。
func appendFinance(out *Summary, row Row, currencies map[string]*CurrencyTotal,
	dimensions map[string]*CostDimension) error {
	if row.Kind == "reimbursement_unknown" {
		out.Unknown++
		return nil
	}
	if row.Kind == "recognition_pending" {
		out.Pending++
		return nil
	}
	if row.Kind == "recognition_conflict" {
		out.Conflicts++
		return nil
	}
	if row.AmountMinor == nil {
		return ErrInvalid
	}
	value := currencies[row.Currency]
	if value == nil {
		value = &CurrencyTotal{Currency: row.Currency}
		currencies[row.Currency] = value
	}
	var target, tax *int64
	switch row.Kind {
	case "recognized_income":
		target, tax = &value.IncomeMinor, &value.IncomeTaxMinor
	case "recognized_cost":
		target, tax = &value.CostMinor, &value.CostTaxMinor
	case "reimbursement_spend":
		target = &value.SpendMinor
	case "prepayment_spend":
		target = &value.PrepaymentMinor
	default:
		return ErrInvalid
	}
	if err := checkedAdd(target, *row.AmountMinor); err != nil {
		return err
	}
	if tax != nil && row.TaxMinor != nil {
		if err := checkedAdd(tax, *row.TaxMinor); err != nil {
			return err
		}
	}
	if row.Kind == "recognized_cost" {
		return appendCostDimensions(row, dimensions)
	}
	return nil
}

// appendCostDimensions 按明确承担组织、归属人员和项目分别汇总净确认成本。
// 参数：row为确认成本，values为当前查询的维度索引。
// 返回值：累计安全时nil，越界时错误。
func appendCostDimensions(row Row, values map[string]*CostDimension) error {
	for _, dimension := range []struct{ kind, id, name string }{
		{"organization", row.OrganizationID, row.OrganizationName},
		{"identity", row.IdentityID, row.IdentityName}, {"project", row.ProjectID, row.ProjectName},
	} {
		key := dimension.kind + "\x00" + dimension.id + "\x00" + row.Currency
		value := values[key]
		if value == nil {
			value = &CostDimension{Dimension: dimension.kind, ID: dimension.id,
				Name: dimension.name, Currency: row.Currency}
			values[key] = value
		}
		if err := checkedAdd(&value.AmountMinor, *row.AmountMinor); err != nil {
			return err
		}
	}
	return nil
}
