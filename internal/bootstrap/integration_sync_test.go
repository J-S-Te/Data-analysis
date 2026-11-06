package bootstrap

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestLoadIntegrationSyncConfiguration 验证API配置默认兼容，关闭与其他能力独立，非法值失败。
// 参数：t 为测试上下文。
// 返回值：无；配置语义不符时失败。
func TestLoadIntegrationSyncConfiguration(t *testing.T) {
	t.Setenv("OIDC_CODEC_KEY", testCodecKeyHex)
	for _, tc := range []struct {
		raw              string
		enabled, invalid bool
	}{{"", true, false}, {"true", true, false}, {"false", false, false}, {"1", false, true}, {"invalid", false, true}} {
		t.Setenv("DASHBOARD_INTEGRATION_SYNC_ENABLED", tc.raw)
		t.Setenv("DASHBOARD_FINANCE_SOURCE", "standard")
		t.Setenv("DASHBOARD_EMBED_ENABLED", "false")
		config, err := LoadConfig()
		if tc.invalid {
			if err == nil {
				t.Fatalf("invalid flag %q accepted", tc.raw)
			}
			continue
		}
		if err != nil || config.IntegrationSyncEnabled != tc.enabled || config.FinanceSource != "standard" || config.DashboardEmbedEnabled {
			t.Fatalf("raw=%q enabled=%v err=%v", tc.raw, config.IntegrationSyncEnabled, err)
		}
	}
}

// TestIntegrationSyncGateRejectsBeforeHandler 验证关闭时管理来源和手动入队明确拒绝且不进入业务处理。
// 参数：t 为测试上下文。
// 返回值：无；关闭模式调用业务处理器或历史读被拒绝时失败。
func TestIntegrationSyncGateRejectsBeforeHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, enabled := range []bool{false, true} {
		router := gin.New()
		calls := 0
		handler := func(c *gin.Context) { calls++; c.Status(http.StatusNoContent) }
		for _, route := range []struct{ method, path string }{{"GET", "/admin/sources"}, {"POST", "/admin/sources/source/trigger"}, {"POST", "/admin/sources/trigger-all"}} {
			router.Handle(route.method, route.path, requireIntegrationSync(enabled), handler)
		}
		router.GET("/dashboard/finance", handler)
		for _, route := range []struct{ method, path string }{{"GET", "/admin/sources"}, {"POST", "/admin/sources/source/trigger"}, {"POST", "/admin/sources/trigger-all"}} {
			record := httptest.NewRecorder()
			router.ServeHTTP(record, httptest.NewRequest(route.method, route.path, nil))
			if enabled && record.Code != http.StatusNoContent {
				t.Fatalf("enabled status=%d", record.Code)
			}
			if !enabled && (record.Code != http.StatusServiceUnavailable || !strings.Contains(record.Body.String(), "INTEGRATION_SYNC_DISABLED")) {
				t.Fatalf("disabled=%d %s", record.Code, record.Body.String())
			}
		}
		expected := 0
		if enabled {
			expected = 3
		}
		if calls != expected {
			t.Fatalf("external handler calls=%d", calls)
		}
		record := httptest.NewRecorder()
		router.ServeHTTP(record, httptest.NewRequest("GET", "/dashboard/finance", nil))
		if record.Code != http.StatusNoContent {
			t.Fatal("historical finance read was gated")
		}
	}
}
