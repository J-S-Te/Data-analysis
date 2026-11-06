package commercial

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestStandardFinanceStatusRetainsHistoryRecovery 核验只读许可仍允许核对已发布批次。
// 参数：t为测试上下文。
// 返回值：无；状态被写门禁拦截或预览发布绕过写门禁时失败。
func TestStandardFinanceStatusRetainsHistoryRecovery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	checked := ""
	router.Use(Middleware(func(_ context.Context, operation string) error {
		checked = operation
		if operation == ReadHistory {
			return nil
		}
		return errors.New("read-only license")
	}))
	for _, path := range []string{"status", "preview", "publish", "status-extra"} {
		router.Any("/data_analysis/api/v1/admin/standard-finance/"+path, func(c *gin.Context) {
			c.Status(http.StatusOK)
		})
	}
	for _, item := range []struct {
		method, path, operation string
		status                  int
	}{
		{http.MethodGet, "status", ReadHistory, 200},
		{http.MethodHead, "status", ReadHistory, 200},
		{http.MethodPost, "status", MutateBusiness, 403},
		{http.MethodPost, "preview", MutateBusiness, 403},
		{http.MethodPost, "publish", MutateBusiness, 403},
		{http.MethodGet, "status-extra", MutateBusiness, 403},
	} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(item.method,
			"/data_analysis/api/v1/admin/standard-finance/"+item.path, nil))
		if response.Code != item.status || checked != item.operation {
			t.Errorf("%s %s: status=%d operation=%s", item.method, item.path, response.Code, checked)
		}
	}
}
