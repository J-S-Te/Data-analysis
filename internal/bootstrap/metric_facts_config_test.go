package bootstrap

import "testing"

// TestMetricFactsAPIRejectsInvalidSwitch 验证API与采集进程使用相同严格开关。
// 参数：t为配置测试上下文。
// 返回值：无；API忽略非法指标开关时失败。
func TestMetricFactsAPIRejectsInvalidSwitch(t *testing.T) {
	t.Setenv("OIDC_CODEC_KEY", testCodecKeyHex)
	t.Setenv("METRIC_FACTS_SYNC_ENABLED", "invalid")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("API silently ignored invalid metric facts switch")
	}
}
