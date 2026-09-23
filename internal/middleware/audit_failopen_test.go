package middleware

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/unified-identity-auth-platform/data-analysis/internal/platformaudit"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

// failingReporterStub 模拟平台审计 ingest 失败（凭据失效、网络故障等）。
type failingReporterStub struct {
	err    error
	calls  int
	events []platformaudit.Event
}

func (s *failingReporterStub) Report(_ context.Context, event platformaudit.Event) error {
	s.calls++
	s.events = append(s.events, event)
	return s.err
}

// newAuditTestRouter 组装与 bootstrap 一致的中间件链：RequestID → 主体注入 → AuditWrites。
func newAuditTestRouter(reporter platformaudit.Reporter, logger *slog.Logger, handler gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequestID(), func(c *gin.Context) {
		c.Request = c.Request.WithContext(auth.WithPrincipal(c.Request.Context(), auth.Principal{UserID: "user-1", DisplayName: "User One"}))
		c.Next()
	}, AuditWrites(reporter, logger))
	router.POST("/api/v1/alerts/:id/close", handler)
	return router
}

// SEC-D4b：强制审计模式下 Report 失败必须拒绝写请求（503），
// 业务响应被丢弃；日志包含路由与请求 id。
func TestAuditWritesRejectsWriteWhenReportFailsUnderRequiredAudit(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	reporter := &failingReporterStub{err: errors.New("platform audit ingest unavailable")}
	executed := false
	router := newAuditTestRouter(reporter, logger, func(c *gin.Context) {
		executed = true
		c.JSON(http.StatusAccepted, gin.H{"closed": true})
	})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/alerts/alert-1/close", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusServiceUnavailable, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "AUDIT_WRITE_REJECTED") {
		t.Fatalf("body = %s, want AUDIT_WRITE_REJECTED", recorder.Body.String())
	}
	if !executed {
		t.Fatal("handler should have executed before the audit decision")
	}
	if reporter.calls != 1 {
		t.Fatalf("Report calls = %d, want 1", reporter.calls)
	}
	logged := logs.String()
	if !strings.Contains(logged, "report platform audit failed") {
		t.Fatalf("missing report failure log: %s", logged)
	}
	if !strings.Contains(logged, "POST /api/v1/alerts/alert-1/close") || !strings.Contains(logged, "request_id") {
		t.Fatalf("failure log must contain route and request id: %s", logged)
	}
}

// SEC-D4b：非强制模式下 Report 失败不改变业务结果，但必须留下含路由与请求 id
// 的 error 日志（原来 logger 为 nil 时完全静默）。
func TestAuditWritesLogsReportFailureWhenAuditNotRequired(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "false")
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	reporter := &failingReporterStub{err: errors.New("platform audit ingest unavailable")}
	router := newAuditTestRouter(reporter, logger, func(c *gin.Context) {
		c.JSON(http.StatusAccepted, gin.H{"closed": true})
	})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/alerts/alert-1/close", nil))

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d (business outcome unchanged); body = %s", recorder.Code, http.StatusAccepted, recorder.Body.String())
	}
	logged := logs.String()
	if !strings.Contains(logged, "report platform audit failed") {
		t.Fatalf("missing report failure log: %s", logged)
	}
	if !strings.Contains(logged, "POST /api/v1/alerts/alert-1/close") || !strings.Contains(logged, "request_id") {
		t.Fatalf("failure log must contain route and request id: %s", logged)
	}
}

// SEC-D4b：强制审计模式下 reporter 缺失时必须在执行业务 handler 之前拒绝写入。
func TestAuditWritesRejectsWhenReporterMissingUnderRequiredAudit(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	executed := false
	router := newAuditTestRouter(nil, logger, func(c *gin.Context) {
		executed = true
		c.JSON(http.StatusAccepted, gin.H{"closed": true})
	})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/alerts/alert-1/close", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusServiceUnavailable, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "AUDIT_UNAVAILABLE") {
		t.Fatalf("body = %s, want AUDIT_UNAVAILABLE", recorder.Body.String())
	}
	if executed {
		t.Fatal("business handler must not run when audit is required but unavailable")
	}
	logged := logs.String()
	if !strings.Contains(logged, "audit reporter unavailable, rejecting request") {
		t.Fatalf("missing reject log: %s", logged)
	}
	if !strings.Contains(logged, "POST /api/v1/alerts/alert-1/close") || !strings.Contains(logged, "request_id") {
		t.Fatalf("reject log must contain route and request id: %s", logged)
	}
}

// SEC-D4b：强制审计模式下上报成功时，缓冲的业务响应必须原样提交。
func TestAuditWritesSuccessFlushesBusinessResponseUnderRequiredAudit(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	reporter := &failingReporterStub{}
	router := newAuditTestRouter(reporter, logger, func(c *gin.Context) {
		c.JSON(http.StatusAccepted, gin.H{"closed": true})
	})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/alerts/alert-1/close", nil))

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusAccepted, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "\"closed\":true") {
		t.Fatalf("business body not flushed: %s", recorder.Body.String())
	}
	if got := recorder.Header().Get("X-Request-ID"); got == "" {
		t.Fatal("pre-swap X-Request-ID header lost after flush")
	}
	if len(reporter.events) != 1 || reporter.events[0].Result != "SUCCESS" {
		t.Fatalf("unexpected events: %#v", reporter.events)
	}
	if strings.Contains(logs.String(), "report platform audit failed") {
		t.Fatalf("unexpected failure log: %s", logs.String())
	}
}
