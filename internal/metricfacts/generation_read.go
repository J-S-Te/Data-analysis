package metricfacts

import (
	"errors"
	"sort"

	"gorm.io/gorm"
)

// generationQuery 在数据库汇总前固定租户、来源、代次和可信数据范围。
// 参数：tx 为一致性只读事务，row 为已发布快照，section/period 为查询，scope 为在线授权。
// 返回值：只包含可见事实的查询；范围歧义或非法期间返回错误。
func generationQuery(tx *gorm.DB, row SnapshotRow, section, period string, scope Scope) (*gorm.DB, error) {
	if err := validateScope(scope); err != nil {
		return nil, err
	}
	if SourceFor(section) != row.SourceCode || period != "" && !ValidPeriod(period) {
		return nil, ErrInvalid
	}
	query := tx.Model(&FactRow{}).Where("generation_id=? AND tenant_id=? AND source_code=?", row.GenerationID, row.TenantID, row.SourceCode)
	switch scope.Mode {
	case "SELF":
		query = query.Where("identity_id=?", scope.UserID)
	case "ORG", "ORG_SUBTREE", "TEAM":
		ids, err := scopeOrganizations(scope)
		if err != nil || len(ids) == 0 {
			return nil, ErrScope
		}
		query = query.Where("organization_id IN ?", ids)
	}
	switch section {
	case "contract":
		query = query.Where("kind=?", "discount")
	case "project":
		query = query.Where("kind IN ?", []string{"person_utilization", "equipment_utilization"})
	case "report":
		query = query.Where("kind IN ?", []string{"report_review", "report_return"})
	case "finance":
		query = query.Where("kind IN ?", []string{"recognized_income", "recognized_cost", "reimbursement_spend", "prepayment_spend", "reimbursement_unknown", "recognition_pending", "recognition_conflict"})
	}
	if period != "" {
		if section == "contract" {
			query = query.Where("period=? OR period=''", period)
		} else if section == "report" {
			query = query.Where("period=? OR (period='' AND kind='report_review' AND status IN ('UNKNOWN','PENDING'))", period)
		} else {
			query = query.Where("period=?", period)
		}
	}
	return query, nil
}

// generationSummary 直接汇总索引事实，不解析整包 JSON 或重新验证全部历史事实。
// 参数：tx 为一致性读事务，row/bundle 为已验证元数据，section/period/scope 为可信查询。
// 返回值：与原完整快照相同口径的统计；范围、累计越界或数据库失败返回错误。
func generationSummary(tx *gorm.DB, row SnapshotRow, bundle Bundle, section, period string, scope Scope) (Summary, error) {
	query, err := generationQuery(tx, row, section, period, scope)
	if err != nil {
		return Summary{}, err
	}
	out := emptySummary(bundle, section, period)
	if err := generationCounts(query, &out); err != nil {
		return Summary{}, err
	}
	switch section {
	case "project":
		err = generationUtilization(query, &out)
	case "finance":
		err = generationFinance(query, &out)
	case "report":
		err = query.Session(&gorm.Session{}).Where("kind=?", "report_return").Distinct("subject_id").Count(&out.ReturnedReports).Error
	}
	if err != nil {
		return Summary{}, err
	}
	out.FirstPassRate = percentage(out.FirstPassed, out.FirstReviewed)
	out.Person.Rate = percentage(out.Person.ActualMinutes, out.Person.CapacityMinutes)
	out.Equipment.Rate = percentage(out.Equipment.ActualMinutes, out.Equipment.CapacityMinutes)
	return out, nil
}

// emptySummary 初始化仅包含来源元数据的汇总，供两种存储协议共用。
// 参数：bundle 为元数据，section/period 为查询分区和期间。
// 返回值：没有事实贡献但真实可用的空统计。
func emptySummary(bundle Bundle, section, period string) Summary {
	return Summary{Available: true, Section: section, Period: period, Basis: bundle.Basis,
		SourceCode: bundle.SourceCode, SnapshotAt: bundle.SnapshotAt, SourceUpdatedAt: bundle.SourceUpdatedAt,
		CollectionStart: bundle.CollectionStartedAt, DiscountBuckets: map[string]int64{}, ReturnReasons: map[string]int64{},
		Currencies: []CurrencyTotal{}, Costs: []CostDimension{}}
}

// generationCounts 只读取分类计数，保留未知月份和首轮证据的原有语义。
// 参数：query 为完成范围/月份过滤的查询，out 为当前统计。
// 返回值：数据库或计数越界错误。
func generationCounts(query *gorm.DB, out *Summary) error {
	if out.Section == "project" {
		return nil
	}
	var groups []struct {
		Kind, Status, Period, Bucket, ReasonCode string
		N                                        int64
	}
	err := query.Session(&gorm.Session{}).Select("kind,status,period,bucket,reason_code,COUNT(*) AS n").
		Group("kind,status,period,bucket,reason_code").Scan(&groups).Error
	if err != nil {
		return err
	}
	for _, group := range groups {
		if group.N < 0 || group.N > MaxInteger {
			return ErrInvalid
		}
		switch out.Section {
		case "contract":
			if out.Period != "" && group.Period == "" {
				out.Unknown += group.N
			} else if group.Status == "KNOWN" {
				out.DiscountKnown += group.N
				out.DiscountBuckets[group.Bucket] += group.N
			} else {
				out.DiscountUnknown += group.N
			}
		case "report":
			if group.Kind == "report_return" {
				out.ReturnedDecisions += group.N
				out.ReturnReasons[group.ReasonCode] += group.N
			} else {
				switch group.Status {
				case "PASSED":
					out.FirstPassed += group.N
					out.FirstReviewed += group.N
				case "RETURNED":
					out.FirstReviewed += group.N
				case "PENDING":
					out.Pending += group.N
				case "UNKNOWN":
					out.Unknown += group.N
				}
			}
		case "finance":
			switch group.Kind {
			case "reimbursement_unknown":
				out.Unknown += group.N
			case "recognition_pending":
				out.Pending += group.N
			case "recognition_conflict":
				out.Conflicts += group.N
			}
		}
	}
	for _, n := range []int64{out.DiscountKnown, out.DiscountUnknown, out.Unknown, out.Pending, out.Conflicts, out.FirstPassed, out.FirstReviewed, out.ReturnedDecisions} {
		if n < 0 || n > MaxInteger {
			return ErrInvalid
		}
	}
	return nil
}

// generationUtilization 以已校验分钟的数据库分组结果计算资源利用率。
// 参数：query 为范围内项目事实，out 为项目统计。
// 返回值：分钟汇总越界或数据库错误。
func generationUtilization(query *gorm.DB, out *Summary) error {
	var groups []struct {
		Kind                               string
		Actual, Capacity, Pending, Unknown int64
		AutoMinutes, AutoCount             int64
	}
	err := query.Session(&gorm.Session{}).Select("kind,SUM(actual_minutes) AS actual,SUM(capacity_minutes) AS capacity,SUM(pending_count) AS pending,SUM(unknown_count) AS unknown,SUM(auto_confirmed_minutes) AS auto_minutes,SUM(auto_confirmed_count) AS auto_count").Group("kind").Scan(&groups).Error
	if err != nil {
		return err
	}
	for _, g := range groups {
		target := &out.Person
		if g.Kind == "equipment_utilization" {
			target = &out.Equipment
		}
		if err := appendUtilization(target, Row{ActualMinutes: &g.Actual, CapacityMinutes: &g.Capacity, PendingCount: &g.Pending, UnknownCount: &g.Unknown, AutoConfirmedMinutes: g.AutoMinutes, AutoConfirmedCount: g.AutoCount}); err != nil {
			return err
		}
	}
	return nil
}

// generationFinance 按币种汇总确认与现金，维度统计只查询确认成本。
// 参数：query 为范围与月份过滤后的财务事实，out 为财务统计。
// 返回值：金额越界、数据库错误或缺失已校验事实时错误。
func generationFinance(query *gorm.DB, out *Summary) error {
	var groups []struct {
		Kind, Currency string
		Amount, Tax    int64
	}
	err := query.Session(&gorm.Session{}).Where("kind IN ?", []string{"recognized_income", "recognized_cost", "reimbursement_spend", "prepayment_spend"}).
		Select("kind,currency,SUM(amount_minor) AS amount,COALESCE(SUM(tax_minor),0) AS tax").Group("kind,currency").Scan(&groups).Error
	if err != nil {
		return err
	}
	currencies := map[string]*CurrencyTotal{}
	for _, g := range groups {
		if err := appendFinance(out, Row{Kind: g.Kind, Currency: g.Currency, AmountMinor: &g.Amount, TaxMinor: &g.Tax}, currencies, map[string]*CostDimension{}); err != nil {
			return err
		}
	}
	for _, v := range currencies {
		out.Currencies = append(out.Currencies, *v)
	}
	sort.Slice(out.Currencies, func(i, j int) bool { return out.Currencies[i].Currency < out.Currencies[j].Currency })
	return generationCosts(query, out)
}

// generationCosts 分别汇总冻结组织、人员及项目，标签变更时选择确定的可见历史名称。
// 参数：query 为已授权的财务查询，out 为成本维度结果。
// 返回值：数据库错误或不精确累计金额错误。
func generationCosts(query *gorm.DB, out *Summary) error {
	for _, dimension := range []string{"organization", "identity", "project"} {
		var groups []struct {
			ID, Name, Currency string
			Amount             int64
		}
		// dimension 来自内部常量，不能由请求拼接列名；不改变可信范围条件。
		err := query.Session(&gorm.Session{}).Where("kind=?", "recognized_cost").
			Select(dimension + "_id AS id,MAX(" + dimension + "_name) AS name,currency,SUM(amount_minor) AS amount").
			Group(dimension + "_id,currency").Order(dimension + "_id,currency").Scan(&groups).Error
		if err != nil {
			return err
		}
		for _, g := range groups {
			if g.Amount > MaxInteger || g.Amount < -MaxInteger {
				return errors.New("指标累计超过可精确展示的范围")
			}
			out.Costs = append(out.Costs, CostDimension{Dimension: dimension, ID: g.ID, Name: g.Name, Currency: g.Currency, AmountMinor: g.Amount})
		}
	}
	sort.Slice(out.Costs, func(i, j int) bool {
		a, b := out.Costs[i], out.Costs[j]
		return a.Dimension+"\x00"+a.ID+"\x00"+a.Currency < b.Dimension+"\x00"+b.ID+"\x00"+b.Currency
	})
	return nil
}
