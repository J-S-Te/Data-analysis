package middleware

import "testing"

// TestAuditRequired 锁定强制审计开关的失败关闭语义：
// PLATFORM_AUDIT_REQUIRED 非空但无法解析时必须视为强制审计，而不是静默关闭；
// 为空时仍按 PLATFORM_ENVIRONMENT_CODE=prod 兜底。
func TestAuditRequired(t *testing.T) {
	for _, tc := range []struct {
		name     string
		explicit string
		envCode  string
		want     bool
	}{
		{"invalid explicit value fails closed outside prod", "notabool", "dev", true},
		{"empty explicit value falls back to non-prod", "", "dev", false},
		{"empty explicit value falls back to prod", "", "prod", true},
		{"explicit false wins over prod", "false", "prod", false},
		{"explicit true wins over non-prod", "true", "dev", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PLATFORM_AUDIT_REQUIRED", tc.explicit)
			t.Setenv("PLATFORM_ENVIRONMENT_CODE", tc.envCode)
			if got := auditRequired(); got != tc.want {
				t.Fatalf("auditRequired() = %v, want %v (explicit=%q env=%q)", got, tc.want, tc.explicit, tc.envCode)
			}
		})
	}
}
