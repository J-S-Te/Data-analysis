package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/unified-identity-auth-platform/data-analysis/internal/platformaudit"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

type auditReporterStub struct{ events []platformaudit.Event }

func (s *auditReporterStub) Report(_ context.Context, event platformaudit.Event) error {
	s.events = append(s.events, event)
	return nil
}

func TestAuditWritesReportsWriteOutcomeAndSkipsReads(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reporter := &auditReporterStub{}
	router := gin.New()
	router.Use(RequestID(), func(c *gin.Context) {
		c.Request = c.Request.WithContext(auth.WithPrincipal(c.Request.Context(), auth.Principal{UserID: "user-1", DisplayName: "User One"}))
		c.Next()
	}, AuditWrites(reporter, nil))
	router.POST("/api/v1/alerts/:id/close", func(c *gin.Context) { c.Status(http.StatusForbidden) })
	router.GET("/api/v1/alerts", func(c *gin.Context) { c.Status(http.StatusOK) })
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/alerts/alert-1/close", nil))
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/alerts", nil))
	if len(reporter.events) != 1 {
		t.Fatalf("events = %d, want 1", len(reporter.events))
	}
	event := reporter.events[0]
	if event.ActorID != "user-1" || event.ResourceType != "ALERT" || event.ResourceID != "alert-1" || event.Result != "FAILURE" || event.ReasonCode != "403" {
		t.Fatalf("unexpected event: %#v", event)
	}
}
