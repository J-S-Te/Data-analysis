package bootstrap

import (
	"testing"

	dashboardinfrastructure "github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/infrastructure"
)

// 64 位十六进制：满足 OIDC_CODEC_KEY 的 32 字节要求，便于聚焦时区校验。
const testCodecKeyHex = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

// TestLoadConfigDefaultsDashboardTimezone 验证未配置时业务日时区默认 Asia/Shanghai。
// 参数：t 测试上下文。
// 返回值：无。
func TestLoadConfigDefaultsDashboardTimezone(t *testing.T) {
	t.Setenv("OIDC_CODEC_KEY", testCodecKeyHex)
	t.Setenv("DASHBOARD_TIMEZONE", "")
	config, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if config.DashboardTimezone != DefaultDashboardTimezone {
		t.Fatalf("DashboardTimezone = %q, want %q", config.DashboardTimezone, DefaultDashboardTimezone)
	}
}

// TestLoadConfigAcceptsExplicitDashboardTimezone 验证显式配置的合法 IANA 时区被采用。
// 参数：t 测试上下文。
// 返回值：无。
func TestLoadConfigAcceptsExplicitDashboardTimezone(t *testing.T) {
	t.Setenv("OIDC_CODEC_KEY", testCodecKeyHex)
	t.Setenv("DASHBOARD_TIMEZONE", "America/New_York")
	config, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if config.DashboardTimezone != "America/New_York" {
		t.Fatalf("DashboardTimezone = %q, want America/New_York", config.DashboardTimezone)
	}
}

// TestLoadConfigRejectsInvalidDashboardTimezone 验证非法时区让启动失败关闭（fail-closed）。
// 参数：t 测试上下文。
// 返回值：无。
func TestLoadConfigRejectsInvalidDashboardTimezone(t *testing.T) {
	t.Setenv("OIDC_CODEC_KEY", testCodecKeyHex)
	t.Setenv("DASHBOARD_TIMEZONE", "Mars/Olympus_Mons")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("LoadConfig() error = nil, want invalid DASHBOARD_TIMEZONE rejection")
	}
}

// TestNewRejectsInvalidDashboardTimezone 验证装配入口在触碰数据库/许可前就失败关闭。
// 参数：t 测试上下文。
// 返回值：无。
func TestNewRejectsInvalidDashboardTimezone(t *testing.T) {
	if _, err := New(Config{DashboardTimezone: "Not/AZone"}); err == nil {
		t.Fatal("New() error = nil, want invalid DASHBOARD_TIMEZONE rejection")
	}
}

// TestLoadDashboardLocationRejectsInvalidZone 验证直接解析入口同样拒绝非法时区。
// 参数：t 测试上下文。
// 返回值：无。
func TestLoadDashboardLocationRejectsInvalidZone(t *testing.T) {
	location, err := LoadDashboardLocation("")
	if err != nil {
		t.Fatalf("LoadDashboardLocation(\"\") error = %v", err)
	}
	if location.String() != DefaultDashboardTimezone {
		t.Fatalf("default location = %v, want %s", location, DefaultDashboardTimezone)
	}
	if _, err := LoadDashboardLocation("Not/AZone"); err == nil {
		t.Fatal("LoadDashboardLocation(Not/AZone) error = nil, want rejection")
	}
}

// TestLoadConfigApprovalSource 验证 DASHBOARD_APPROVAL_SOURCE 的装载口径：
// 空值默认 status（行为不变）、instances 被接受、非法值启动失败。
// 参数：t 测试上下文。
// 返回值：无。
func TestLoadConfigApprovalSource(t *testing.T) {
	cases := []struct {
		raw     string
		want    dashboardinfrastructure.ApprovalSource
		wantErr bool
	}{
		{raw: "", want: dashboardinfrastructure.ApprovalSourceStatus},
		{raw: "status", want: dashboardinfrastructure.ApprovalSourceStatus},
		{raw: "instances", want: dashboardinfrastructure.ApprovalSourceInstances},
		{raw: "INSTANCES", want: dashboardinfrastructure.ApprovalSourceInstances},
		{raw: "pending", wantErr: true},
	}
	for _, tc := range cases {
		t.Setenv("OIDC_CODEC_KEY", testCodecKeyHex)
		t.Setenv("DASHBOARD_APPROVAL_SOURCE", tc.raw)
		config, err := LoadConfig()
		if tc.wantErr {
			if err == nil {
				t.Fatalf("LoadConfig() error = nil for DASHBOARD_APPROVAL_SOURCE=%q, want rejection", tc.raw)
			}
			continue
		}
		if err != nil {
			t.Fatalf("LoadConfig() error = %v for %q", err, tc.raw)
		}
		if config.DashboardApprovalSource != tc.want {
			t.Fatalf("DashboardApprovalSource = %q, want %q", config.DashboardApprovalSource, tc.want)
		}
	}
}
