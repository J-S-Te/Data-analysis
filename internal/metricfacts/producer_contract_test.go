package metricfacts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMetricSourceProducerBundles 验证隔离业务生产者真实快照可被消费者严格解码。
// 参数：t为测试上下文，环境目录只读取三个Writer输出的合成业务快照。
// 返回值：无；任一生产者包络、事实字段或独立口径不兼容时失败。
func TestMetricSourceProducerBundles(t *testing.T) {
	dir := os.Getenv("METRIC_SOURCE_FIXTURE_DIRECTORY")
	if dir == "" {
		t.Skip("未提供隔离业务生产者快照目录")
	}
	if !filepath.IsAbs(dir) {
		t.Fatal("生产者快照目录必须为绝对路径")
	}
	for _, test := range []struct{ file, source, section string }{
		{"task-1-metric-fixture.json", "project_management", "project"},
		{"task-2-metric-fixture.json", "contract_management", "contract"},
	} {
		t.Run(test.source, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(dir, test.file))
			if err != nil {
				t.Fatal(err)
			}
			var identity struct {
				Data struct {
					TenantID string `json:"tenant_id"`
				} `json:"data"`
			}
			if json.Unmarshal(raw, &identity) != nil {
				t.Fatal("生产者包络不可解析")
			}
			bundle, err := Decode(strings.NewReader(string(raw)), identity.Data.TenantID, test.source, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			assertProducerSummary(t, bundle, test.section)
		})
	}
}

// assertProducerSummary 对真实生产者合成场景核验独立业务预期。
// 参数：t为测试上下文，bundle为已严格解码快照，section为场景分区。
// 返回值：无；原八折合同或一小时实际/四小时容量口径不符时失败。
func assertProducerSummary(t *testing.T, bundle Bundle, section string) {
	t.Helper()
	out, err := Summarize(bundle, section, "", Scope{Mode: "TENANT"})
	if err != nil {
		t.Fatal(err)
	}
	if section == "contract" && (out.DiscountKnown != 1 || out.DiscountUnknown != 1 ||
		out.DiscountBuckets["FROM_80_TO_90"] != 1) {
		t.Fatalf("八折及旧合同未知预期不符: %+v", out)
	}
	if section == "project" && (out.Person.ActualMinutes != 60 ||
		out.Person.CapacityMinutes != 240 || out.Person.Rate == nil || *out.Person.Rate != 25) {
		t.Fatalf("真实容量/确认实际预期不符: %+v", out)
	}
}

// TestFinanceSourceProducerScenarios 验证财务真实生产者十个独立场景及净税冲回边界。
// 参数：t为测试上下文，fixture目录为已授权的隔离生产者输出。
// 返回值：无；解码、跨期、确认额、未知或现金重复累计时失败。
func TestFinanceSourceProducerScenarios(t *testing.T) {
	dir := os.Getenv("METRIC_SOURCE_FIXTURE_DIRECTORY")
	if dir == "" {
		t.Skip("未提供隔离业务生产者快照目录")
	}
	if !filepath.IsAbs(dir) {
		t.Fatal("生产者快照目录必须为绝对路径")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "task-3-metric-fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixtures struct {
		Scenarios map[string]Bundle `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name                                     string
		income, cost, prepay, unknown, conflicts int64
		spend, costTax                           int64
	}{
		{"income_100_cost_40_confirmed", 100, 40, 0, 0, 0, 0, 0},
		{"income_cross_period_reversed", 0, 40, 0, 0, 0, 0, 0},
		{"prepayment_cash_not_cost", 0, 40, 50, 0, 0, 0, 0},
		{"refund_repaid_still_conflict", 0, 90, 50, 0, 1, 0, 0},
		{"refund_reversal_cleared_repayment_draft", 0, 40, 50, 0, 0, 0, 0},
		{"historical_payment_unknown", 0, 40, 0, 3, 0, 0, 0},
		{"mixed_real_reimbursement_effects", 0, 0, 0, 0, 0, 0, 0},
		{"mixed_known_and_unknown_history", 0, 0, 0, 2, 0, 0, 0},
		{"residual_tax_pending", 0, 0, 0, 0, 1, 106, 6},
		{"residual_tax_cleared", 0, 0, 0, 0, 0, 106, 0},
	}
	if len(fixtures.Scenarios) != len(tests) {
		t.Fatalf("场景数错误: %d", len(fixtures.Scenarios))
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source, ok := fixtures.Scenarios[test.name]
			if !ok {
				t.Fatal("缺少生产者场景")
			}
			envelope, err := json.Marshal(map[string]any{"code": "OK", "data": source})
			if err != nil {
				t.Fatal(err)
			}
			bundle, err := Decode(strings.NewReader(string(envelope)), source.TenantID, "settlement", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			summary, err := Summarize(bundle, "finance", "", Scope{Mode: "TENANT"})
			if err != nil {
				t.Fatal(err)
			}
			value := CurrencyTotal{}
			if len(summary.Currencies) > 0 {
				if len(summary.Currencies) != 1 || summary.Currencies[0].Currency != "CNY" {
					t.Fatal("币种错误")
				}
				value = summary.Currencies[0]
			}
			if value.IncomeMinor != test.income || value.CostMinor != test.cost ||
				value.PrepaymentMinor != test.prepay || value.SpendMinor != test.spend ||
				value.CostTaxMinor != test.costTax || summary.Unknown != test.unknown ||
				summary.Conflicts != test.conflicts {
				t.Fatalf("独立财务预期不符: %+v", summary)
			}
			if strings.HasPrefix(test.name, "residual_tax_") && summary.Pending != 0 {
				t.Fatal("未提交重付草稿不得计待审或确认额")
			}
			if test.name == "residual_tax_cleared" {
				foundTaxOnly := false
				for _, row := range bundle.Rows {
					foundTaxOnly = foundTaxOnly || row.Kind == "recognized_cost" &&
						row.AmountMinor != nil && *row.AmountMinor == 0 &&
						row.TaxMinor != nil && *row.TaxMinor == -6
				}
				if !foundTaxOnly {
					t.Fatal("真实零净额负税额冲回被省略")
				}
			}
			if test.name == "income_cross_period_reversed" {
				for period, want := range map[string]int64{"2026-10": 100, "2026-11": -100} {
					month, err := Summarize(bundle, "finance", period, Scope{Mode: "TENANT"})
					if err != nil || len(month.Currencies) != 1 || month.Currencies[0].IncomeMinor != want {
						t.Fatalf("冲回跨期失败: %+v %v", month, err)
					}
				}
			}
			if strings.HasPrefix(test.name, "mixed_") {
				assertFinanceCashConservation(t, bundle, test.unknown)
			}
		})
	}
}

// assertFinanceCashConservation 检查真实付款分项与冲正无重复且未知不重复计金额。
// 参数：t为测试上下文，bundle为已解码真实源，unknown为预期未知行数。
// 返回值：无；已知六行或原付款101分、未知20分不守恒时失败。
func assertFinanceCashConservation(t *testing.T, bundle Bundle, unknown int64) {
	t.Helper()
	knownRows := 0
	var knownPositive, unknownPositive int64
	for _, row := range bundle.Rows {
		if row.Kind == "reimbursement_unknown" {
			if row.AmountMinor != nil && *row.AmountMinor > 0 {
				unknownPositive += *row.AmountMinor
			}
			continue
		}
		if row.Kind != "reimbursement_spend" && row.Kind != "prepayment_spend" {
			t.Fatal("现金场景混入确认额")
		}
		knownRows++
		if row.AmountMinor != nil && *row.AmountMinor > 0 {
			knownPositive += *row.AmountMinor
		}
	}
	wantUnknown := int64(0)
	if unknown > 0 {
		wantUnknown = 20
	}
	if knownRows != 6 || knownPositive != 101 || unknownPositive != wantUnknown {
		t.Fatalf("付款守恒失败 rows=%d known=%d unknown=%d", knownRows, knownPositive, unknownPositive)
	}
}
