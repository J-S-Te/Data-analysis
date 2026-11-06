package integrationsync

import "testing"

// TestIntegrationSyncFlagRequiresExplicitDisable 验证默认开启及严格配置，关闭不依赖其他能力。
// 参数：t 为测试上下文。
// 返回值：无；非法值或默认兼容语义不符时失败。
func TestIntegrationSyncFlagRequiresExplicitDisable(t *testing.T) {
	for _, tc := range []struct {
		raw              string
		enabled, invalid bool
	}{{"", true, false}, {"true", true, false}, {"false", false, false}, {"yes", false, true}, {"0", false, true}, {" true ", false, true}, {"TRUE", false, true}} {
		enabled, err := EnabledFromEnv(func(key string) string {
			if key != EnvironmentKey {
				t.Fatalf("unexpected key %s", key)
			}
			return tc.raw
		})
		if tc.invalid {
			if err == nil {
				t.Fatalf("invalid flag %q accepted", tc.raw)
			}
			continue
		}
		if err != nil || enabled != tc.enabled {
			t.Fatalf("raw=%q enabled=%v err=%v", tc.raw, enabled, err)
		}
	}
}
