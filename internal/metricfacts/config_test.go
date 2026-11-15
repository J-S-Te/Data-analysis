package metricfacts

import "testing"

// TestMetricFactsEnabledExplicit 验证启用开关默认关闭并拒绝非法配置。
// 参数：t为测试上下文。
// 返回值：无；非法值静默启用或关闭时失败。
func TestMetricFactsEnabledExplicit(t *testing.T) {
	for _, test := range []struct {
		value          string
		enabled, valid bool
	}{
		{"", false, true}, {"false", false, true}, {"true", true, true}, {"typo", false, false},
	} {
		enabled, err := EnabledFromEnv(func(string) string { return test.value })
		if (err == nil) != test.valid || err == nil && enabled != test.enabled {
			t.Fatalf("value=%q enabled=%t err=%v", test.value, enabled, err)
		}
	}
}
