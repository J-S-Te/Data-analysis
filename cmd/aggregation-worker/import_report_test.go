package main

import "testing"

// TestImportReportConfigDefaultClosed 验证默认关闭及显式启用的配置门禁。
// 参数：t 为测试上下文。
// 返回值：无，错误用途、复用客户端和不完整URL不能启动同步。
func TestImportReportConfigDefaultClosed(t *testing.T) {
	t.Setenv("CONTRACT_IMPORT_SYNC_ENABLED", "")
	o, err := importReportOptionsFromEnv()
	if err != nil || o.Enabled {
		t.Fatal("default must remain closed")
	}
	for key, value := range map[string]string{
		"CONTRACT_IMPORT_SYNC_ENABLED": "true", "CONTRACT_IMPORT_SUMMARY_URL": "http://contract-api/internal/v1/contract-imports/report-source", "CONTRACT_IMPORT_SYNC_TOKEN_URL": "http://platform-api/oauth2/token", "CONTRACT_IMPORT_SYNC_CLIENT_ID": "report-reader", "CONTRACT_IMPORT_SYNC_CLIENT_SECRET": "synthetic", "OIDC_TENANT_ID": "t", "CONTRACT_IMPORT_SYNC_SCOPE": "contract.import.report.read", "CONTRACT_MACHINE_CLIENT_ID": "dashboard-reader", "PROJECT_MACHINE_CLIENT_ID": "project-reader",
	} {
		t.Setenv(key, value)
	}
	if _, err = importReportOptionsFromEnv(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ key, value string }{
		{"CONTRACT_IMPORT_SYNC_ENABLED", "maybe"}, {"CONTRACT_IMPORT_SUMMARY_URL", "/relative"}, {"CONTRACT_IMPORT_SYNC_TOKEN_URL", "http://user:pass@host/token"}, {"CONTRACT_IMPORT_SYNC_SCOPE", "dashboard.contract.read"}, {"CONTRACT_IMPORT_SYNC_CLIENT_ID", "dashboard-reader"}, {"OIDC_TENANT_ID", ""},
	} {
		t.Run(tc.key, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if _, err := importReportOptionsFromEnv(); err == nil {
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
}
