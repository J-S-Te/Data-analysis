package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/unified-identity-auth-platform/data-analysis/internal/platformaudit"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

func sessionActivityRouter(reporter platformaudit.Reporter) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequestID(), func(c *gin.Context) {
		c.Request = c.Request.WithContext(auth.WithPrincipal(c.Request.Context(), auth.Principal{UserID: "user-1", DisplayName: "User One"}))
		c.Next()
	}, AuditWrites(reporter, nil, nil))
	router.POST("/api/v1/auth/activity", func(c *gin.Context) { c.Status(http.StatusOK) })
	router.POST("/api/v1/dictionary", func(c *gin.Context) { c.Status(http.StatusOK) })
	return router
}

// TestAuditWritesSkipsSessionActivity 验证会话活动探测不产生平台审计事件，
// 避免前端每 30s 一次的活动上报放大审计写入；业务写入仍然被审计。
func TestAuditWritesSkipsSessionActivity(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "false")
	t.Setenv("PLATFORM_ENVIRONMENT_CODE", "dev")
	reporter := &auditReporterStub{}
	router := sessionActivityRouter(reporter)
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/auth/activity", nil))
	if len(reporter.events) != 0 {
		t.Fatalf("session activity produced %d audit events, want 0", len(reporter.events))
	}
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/dictionary", nil))
	if len(reporter.events) != 1 {
		t.Fatalf("business write produced %d audit events, want 1", len(reporter.events))
	}
}

// TestAuditRequiredModeDoesNotBlockSessionActivity 验证强制审计且 audit reporter 缺失时，
// 活动续期不被审计门禁误伤（否则在线用户会因审计故障在空闲到期前被登出），业务写入仍拒绝。
func TestAuditRequiredModeDoesNotBlockSessionActivity(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	t.Setenv("PLATFORM_ENVIRONMENT_CODE", "prod")
	router := sessionActivityRouter(nil)
	activity := httptest.NewRecorder()
	router.ServeHTTP(activity, httptest.NewRequest(http.MethodPost, "/api/v1/auth/activity", nil))
	if activity.Code != http.StatusOK {
		t.Fatalf("activity status = %d, want 200 without audit reporter", activity.Code)
	}
	business := httptest.NewRecorder()
	router.ServeHTTP(business, httptest.NewRequest(http.MethodPost, "/api/v1/dictionary", nil))
	if business.Code != http.StatusServiceUnavailable {
		t.Fatalf("business write status = %d, want 503 in required audit mode without reporter", business.Code)
	}
}
