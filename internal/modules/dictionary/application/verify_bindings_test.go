package application

import "testing"

// TestControlledBindingCoverage 验证受控绑定表的全部指标在派生后获得绑定与可启用。
// 参数：t 为当前测试上下文。
// 返回值：无；受控指标未获得绑定或不可启用时失败。
func TestControlledBindingCoverage(t *testing.T) {
	for _, metric := range builtinMetrics() {
		if _, controlled := controlledBindings[metric.Code]; controlled && !metric.CanEnable {
			t.Errorf("受控指标 %s 未获得绑定: reason=%s", metric.Code, metric.EnableBlockReason)
		}
	}
}
