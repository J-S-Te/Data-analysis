package bootstrap

import (
	"reflect"
	"testing"
)

// TestLoadConfigFinanceSource 验证财务来源显式选择且默认保持现有联动模式。
// 参数：t 为测试上下文。
// 返回值：无。
func TestLoadConfigFinanceSource(t *testing.T) {
	for _, raw := range []string{"", "integration", "standard", "typo"} {
		t.Setenv("OIDC_CODEC_KEY", testCodecKeyHex)
		t.Setenv("DASHBOARD_FINANCE_SOURCE", raw)
		config, err := LoadConfig()
		if raw == "typo" {
			if err == nil {
				t.Fatal("unknown finance source accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		field := reflect.ValueOf(config).FieldByName("FinanceSource")
		want := raw
		if want == "" {
			want = "integration"
		}
		if !field.IsValid() || field.String() != want {
			t.Fatalf("finance source=%s missing or incorrect", raw)
		}
	}
}
