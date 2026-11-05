package bootstrap

import "testing"

// TestAuditRequiredFromEnvironment 锁定启动期审计必开判定与中间件一致：
// PLATFORM_AUDIT_REQUIRED 非空但无法解析时失败关闭返回 true；为空时按环境码兜底。
func TestAuditRequiredFromEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name     string
		explicit string
		envCode  string
		want     bool
	}{
		{"invalid explicit value fails closed", "notabool", "dev", true},
		{"empty explicit value falls back to non-prod", "", "dev", false},
		{"empty explicit value falls back to prod", "", "prod", true},
		{"explicit false wins over prod", "false", "prod", false},
		{"explicit true wins over non-prod", "true", "dev", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PLATFORM_AUDIT_REQUIRED", tc.explicit)
			t.Setenv("PLATFORM_ENVIRONMENT_CODE", tc.envCode)
			if got := auditRequiredFromEnvironment(); got != tc.want {
				t.Fatalf("auditRequiredFromEnvironment() = %v, want %v (explicit=%q env=%q)", got, tc.want, tc.explicit, tc.envCode)
			}
		})
	}
}
