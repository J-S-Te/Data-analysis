package bootstrap

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestInternalReportStatusHasSingleAuthoritativeMount 验证机器报表状态端点只挂在
// 合同契约声明的根路径；带 APP_PATH_PREFIX 的重复挂载必须不存在。
func TestInternalReportStatusHasSingleAuthoritativeMount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	// 模拟生产装配：先建立 base 组，再挂机器端点。
	_ = router.Group("/data_analysis")
	mountInternalReportStatus(router, func(c *gin.Context) { c.Status(http.StatusNoContent) })

	routes := router.Routes()
	if len(routes) != 1 {
		t.Fatalf("route count=%d, want 1: %+v", len(routes), routes)
	}
	if routes[0].Method != http.MethodGet || routes[0].Path != InternalReportStatusPath {
		t.Fatalf("route=%s %s, want GET %s", routes[0].Method, routes[0].Path, InternalReportStatusPath)
	}

	for _, tc := range []struct {
		path string
		want int
	}{
		{InternalReportStatusPath, http.StatusNoContent},
		{"/data_analysis" + InternalReportStatusPath, http.StatusNotFound},
	} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if response.Code != tc.want {
			t.Fatalf("%s status=%d, want %d", tc.path, response.Code, tc.want)
		}
	}
}
