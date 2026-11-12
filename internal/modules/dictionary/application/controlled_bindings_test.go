package application

import (
	"context"
	"testing"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dictionary/domain"
)

// TestControlledCatalogCoverage 核验目录对已接入与未满足口径的指标作出准确启用判断。
// 参数：t为当前独立测试上下文。
// 返回值：无；身份缺失、错误绑定或默认静默启用时失败。
func TestControlledCatalogCoverage(t *testing.T) {
	expected := map[string]string{
		"1.1": "CONTRACT_SIGNING_FACTS_V1", "1.2": "CONTRACT_SIGNING_FACTS_V1",
		"1.3": "CONTRACT_REGION_FACTS_V1", "1.4": "CONTRACT_CONVERSION_FACTS_V1",
		"1.5": "CONTRACT_DISCOUNT_FACTS_V1", "2.1": "PROJECT_STATUS_COUNTS_V1",
		"2.4": "PERSON_UTILIZATION_FACTS_V1", "2.5": "EQUIPMENT_UTILIZATION_FACTS_V1",
		"3.2": "REPORT_FIRST_REVIEW_FACTS_V1", "3.3": "REPORT_RETURN_FACTS_V1",
		"4.1": "FINANCE_CURRENCY_TOTALS_V1", "4.6": "FINANCE_RECOGNIZED_COST_FACTS_V1",
		"4.7": "FINANCE_RECOGNIZED_COST_FACTS_V1",
	}
	catalog := NewCatalogService().Get()
	if len(catalog.Metrics) != 22 {
		t.Fatalf("目录数量异常：%d", len(catalog.Metrics))
	}
	seen := map[string]bool{}
	for _, metric := range catalog.Metrics {
		binding, enabled := expected[metric.Code]
		if seen[metric.Code] {
			t.Fatalf("重复指标：%s", metric.Code)
		}
		seen[metric.Code] = true
		if metric.CanEnable != enabled || metric.CalculationBinding != binding {
			t.Errorf("%s 启用资格或身份错误：%+v", metric.Code, metric)
		}
		if metric.Enabled != (metric.Code == "2.1") {
			t.Errorf("%s 默认启用状态改变：%t", metric.Code, metric.Enabled)
		}
	}
	for code := range expected {
		if !seen[code] {
			t.Errorf("已接入指标缺失：%s", code)
		}
	}
}

// TestControlledBindingRejectsDrift 验证每项绑定拒绝未确认、自定义及任一计算口径漂移。
// 参数：t为测试上下文，指标来自当前发布目录。
// 返回值：无；非法定义仍能启用或客户端伪造身份被保留时失败。
func TestControlledBindingRejectsDrift(t *testing.T) {
	changes := map[string]func(*domain.Metric){
		"dashboard":  func(m *domain.Metric) { m.Dashboard += "变更" },
		"definition": func(m *domain.Metric) { m.Definition += "变更" },
		"formula":    func(m *domain.Metric) { m.Formula += "变更" },
		"source":     func(m *domain.Metric) { m.Source += "变更" },
		"period":     func(m *domain.Metric) { m.Period += "变更" },
		"status":     func(m *domain.Metric) { m.Status = "待确认" },
		"origin":     func(m *domain.Metric) { m.Origin = "CUSTOM" },
	}
	for _, metric := range builtinMetrics() {
		if !metric.CanEnable {
			continue
		}
		for name, change := range changes {
			t.Run(metric.Code+"/"+name, func(t *testing.T) {
				candidate := metric
				change(&candidate)
				candidate.Enabled, candidate.CanEnable = true, true
				candidate.CalculationBinding = "forged"
				got := derived(candidate)
				if got.Enabled || got.CanEnable || got.CalculationBinding != "" ||
					got.EnableBlockReason == "" {
					t.Fatalf("非法口径获得绑定：%+v", got)
				}
			})
		}
	}
}

// TestControlledBindingTenantState 验证新增资格不覆盖租户禁用及历史口径定义。
// 参数：t为测试上下文，仅使用内存SQLite隔离存储。
// 返回值：无；显式状态、版本冲突、范围隔离或漂移保护不正确时失败。
func TestControlledBindingTenantState(t *testing.T) {
	service := NewCatalogService(testDB(t))
	ctx := context.Background()
	initial, ok := builtin("1.5")
	if !ok || !initial.CanEnable || initial.Enabled {
		t.Fatal("折扣默认状态不正确")
	}
	enabled, err := service.SetEnabled(ctx, "tenant-a", "actor", "1.5", true, 1)
	if err != nil || !enabled.Enabled {
		t.Fatalf("合法启用失败：%+v %v", enabled, err)
	}
	_, err = service.SetEnabled(ctx, "tenant-a", "actor", "1.5", false, 1)
	expectCode(t, err, "METRIC_VERSION_CONFLICT")
	disabled, err := service.SetEnabled(ctx, "tenant-a", "actor", "1.5", false, enabled.Version)
	if err != nil || disabled.Enabled || !disabled.CanEnable {
		t.Fatalf("禁用失败：%+v %v", disabled, err)
	}
	catalog, err := service.ListForTenant(ctx, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, metric := range catalog.Metrics {
		if metric.Code == "1.5" && (metric.Enabled || metric.Version != disabled.Version) {
			t.Fatalf("读取覆盖租户禁用：%+v", metric)
		}
	}
	old, _ := builtin("3.2")
	old.Formula = "首次通过数 / 首次送审数"
	drifted, err := service.Update(ctx, "tenant-a", "actor", old.Code, old)
	if err != nil || drifted.CanEnable || drifted.Enabled {
		t.Fatalf("历史口径误绑定：%+v %v", drifted, err)
	}
	other, err := service.ListForTenant(ctx, "tenant-b")
	if err != nil {
		t.Fatal(err)
	}
	for _, metric := range other.Metrics {
		if metric.Code == "3.2" && (!metric.CanEnable || metric.Enabled || metric.Version != 1) {
			t.Fatalf("租户定义串用：%+v", metric)
		}
	}
}
