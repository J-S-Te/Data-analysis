package commercial

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestMetricFactsHistoryClassification 验证已发布指标只读查询在到期时保留历史读取。
// 参数：t为隔离HTTP路由测试上下文。
// 返回值：无；新只读接口被误当业务写或未知接口被放行时失败。
func TestMetricFactsHistoryClassification(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(Middleware(Check(func(_ context.Context, operation string) error {
		if operation != ReadHistory {
			return errors.New("business write denied")
		}
		return nil
	})))
	for _, path := range []string{"/api/v1/dashboard/metrics", "/api/v1/dashboard/projects", "/api/v1/dashboard/unclassified"} {
		router.GET(path, func(c *gin.Context) { c.Status(200) })
	}
	for _, test := range []struct {
		path   string
		status int
	}{
		{"/api/v1/dashboard/metrics?section=finance", 200},
		{"/api/v1/dashboard/projects", 200}, {"/api/v1/dashboard/unclassified", 403},
	} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest("GET", test.path, nil))
		if response.Code != test.status {
			t.Fatalf("path=%s status=%d", test.path, response.Code)
		}
	}
}
