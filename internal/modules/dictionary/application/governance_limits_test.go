package application

import (
	"strings"
	"testing"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dictionary/domain"
)

func TestMetricValidationRejectsOverlongFields(t *testing.T) {
	base := domain.Metric{Code: "METRIC_1", Name: "指标", Dashboard: "看板", Definition: "定义", Formula: "公式", Source: "来源", Period: "月", Status: "启用"}
	if err := validate(base); err != nil {
		t.Fatalf("valid metric rejected: %v", err)
	}
	cases := []domain.Metric{
		func() domain.Metric { item := base; item.Name = strings.Repeat("名", 129); return item }(),
		func() domain.Metric { item := base; item.Definition = strings.Repeat("定", 513); return item }(),
		func() domain.Metric { item := base; item.Source = strings.Repeat("源", 256); return item }(),
	}
	for index, item := range cases {
		if err := validate(item); err == nil {
			t.Fatalf("case %d unexpectedly accepted", index)
		}
	}
}
