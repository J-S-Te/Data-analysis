package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/unified-identity-auth-platform/data-analysis/internal/commercial"
	"github.com/unified-identity-auth-platform/data-analysis/internal/middleware"
)

const chainPublicOrigin = "https://dashboard.example.test"

// newSessionIdleChain 按 bootstrap/app.go 的实际顺序装配会话相关中间件链：
// SessionAuth -> AuditWrites -> RequireSameOriginWrite -> 商业授权门禁 -> handler。
// 用于覆盖活动接口的鉴权、CSRF、许可白名单与审计豁免；不连接 OIDC Provider。
func newSessionIdleChain(t *testing.T, service *Service) *gin.Engine {
	t.Helper()
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "false")
	t.Setenv("PLATFORM_ENVIRONMENT_CODE", "dev")
	gin.SetMode(gin.TestMode)
	handler := NewHandler(service, HTTPOptions{CookieName: "data_analysis_session", PublicOrigin: chainPublicOrigin})
	licenseCheck := commercial.Check(func(_ context.Context, operation string) error {
		if operation == commercial.MutateBusiness {
			return errors.New("license expired")
		}
		return nil
	})
	router := gin.New()
	router.Use(middleware.RequestID(), middleware.SessionAuth(service, "data_analysis_session"))
	api := router.Group("/data_analysis/api/v1")
	api.Use(middleware.AuditWrites(nil, nil, nil))
	api.Use(middleware.RequireSameOriginWrite(chainPublicOrigin))
	api.Use(commercial.Middleware(licenseCheck))
	api.GET("/auth/me", handler.AuthMe)
	api.POST("/auth/activity", handler.Activity)
	// 业务写路由用于证明商业授权门禁仍然生效（活动接口是唯一豁免）。
	api.POST("/dashboard/stub", func(c *gin.Context) { c.Status(http.StatusOK) })
	return router
}

func newSessionActivityRequest(t *testing.T) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/data_analysis/api/v1/auth/activity", nil)
	request.AddCookie(&http.Cookie{Name: "data_analysis_session", Value: idleTestRawToken})
	request.Header.Set("Origin", chainPublicOrigin)
	request.Header.Set("X-CSRF-Token", "1")
	return request
}

func decodeSessionChainEnvelope(t *testing.T, recorder *httptest.ResponseRecorder) sessionEnvelope {
	t.Helper()
	var payload sessionEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, recorder.Body.String())
	}
	return payload
}

// TestSessionActivityHTTPChainRenewsSession 覆盖活动接口经过真实中间件链后的成功路径：
// 鉴权、同源/CSRF 校验、商业授权白名单（授权门禁拒绝变更为 MutateBusiness 仍放行）都通过。
func TestSessionActivityHTTPChainRenewsSession(t *testing.T) {
	service, db := newSessionIdleTestService(t, 30*time.Minute, 8*time.Hour)
	lastInteractive := time.Now().UTC().Add(-20 * time.Minute)
	seedIdleTestSession(t, db, func(session *Session) {
		session.LastInteractiveAt = &lastInteractive
		session.CreatedAt = lastInteractive
	})
	router := newSessionIdleChain(t, service)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, newSessionActivityRequest(t))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}
	payload := decodeSessionChainEnvelope(t, recorder)
	if payload.Code != "OK" || payload.Data.IdleTimeoutSeconds != 1800 || payload.Data.IdleRemainingMS <= 0 {
		t.Fatalf("unexpected activity payload: %+v", payload)
	}
	after := loadIdleTestSession(t, db)
	if after.LastInteractiveAt == nil || !after.LastInteractiveAt.After(lastInteractive) {
		t.Fatalf("activity chain did not advance last_interactive_at: %v", after.LastInteractiveAt)
	}
}

// TestSessionActivityHTTPChainRequiresSameOriginWrite 验证活动接口沿用 Origin/CSRF 保护。
func TestSessionActivityHTTPChainRequiresSameOriginWrite(t *testing.T) {
	service, db := newSessionIdleTestService(t, 30*time.Minute, 8*time.Hour)
	seedIdleTestSession(t, db, nil)
	router := newSessionIdleChain(t, service)
	request := newSessionActivityRequest(t)
	request.Header.Del("X-CSRF-Token")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 without CSRF header", recorder.Code)
	}
}

// TestSessionActivityHTTPChainRejectsIdleExpired 验证活动接口对空闲到期会话返回 401 且不复活。
func TestSessionActivityHTTPChainRejectsIdleExpired(t *testing.T) {
	service, db := newSessionIdleTestService(t, 30*time.Minute, 8*time.Hour)
	idleExpiredAt := time.Now().UTC().Add(-31 * time.Minute)
	seedIdleTestSession(t, db, func(session *Session) {
		session.LastInteractiveAt = &idleExpiredAt
		session.CreatedAt = idleExpiredAt
	})
	router := newSessionIdleChain(t, service)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, newSessionActivityRequest(t))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
	payload := decodeSessionChainEnvelope(t, recorder)
	if payload.Code != "UNAUTHENTICATED" {
		t.Fatalf("code = %q, want UNAUTHENTICATED", payload.Code)
	}
	after := loadIdleTestSession(t, db)
	if after.LastInteractiveAt == nil || !after.LastInteractiveAt.Equal(idleExpiredAt) {
		t.Fatalf("inactive session was revived through HTTP chain: %v", after.LastInteractiveAt)
	}
}

// TestSessionActivityHTTPChainKeepsBusinessLicenseGate 验证豁免只针对会话接口，业务写仍受许可门禁。
func TestSessionActivityHTTPChainKeepsBusinessLicenseGate(t *testing.T) {
	service, db := newSessionIdleTestService(t, 30*time.Minute, 8*time.Hour)
	seedIdleTestSession(t, db, nil)
	router := newSessionIdleChain(t, service)
	request := httptest.NewRequest(http.MethodPost, "/data_analysis/api/v1/dashboard/stub", nil)
	request.AddCookie(&http.Cookie{Name: "data_analysis_session", Value: idleTestRawToken})
	request.Header.Set("Origin", chainPublicOrigin)
	request.Header.Set("X-CSRF-Token", "1")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("business write status = %d, want 403 from commercial gate", recorder.Code)
	}
}

// TestAuthMeHTTPChainReturnsIdleFields 验证 /auth/me 经由真实认证链追加空闲字段。
func TestAuthMeHTTPChainReturnsIdleFields(t *testing.T) {
	service, db := newSessionIdleTestService(t, 30*time.Minute, 8*time.Hour)
	lastInteractive := time.Now().UTC().Add(-10 * time.Minute)
	seedIdleTestSession(t, db, func(session *Session) {
		session.LastInteractiveAt = &lastInteractive
		session.CreatedAt = lastInteractive
	})
	router := newSessionIdleChain(t, service)
	request := httptest.NewRequest(http.MethodGet, "/data_analysis/api/v1/auth/me", nil)
	request.AddCookie(&http.Cookie{Name: "data_analysis_session", Value: idleTestRawToken})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}
	payload := decodeSessionChainEnvelope(t, recorder)
	if payload.Code != "OK" || payload.Data.UserID != "user-1" || payload.Data.TenantID != "tenant-1" {
		t.Fatalf("unexpected /auth/me payload: %+v", payload)
	}
	if payload.Data.IdleTimeoutSeconds != 1800 || payload.Data.IdleRemainingMS < int64((19*time.Minute)/time.Millisecond) || payload.Data.IdleRemainingMS > int64((20*time.Minute)/time.Millisecond) {
		t.Fatalf("idle fields = %d/%d, want 1800/about 20m", payload.Data.IdleTimeoutSeconds, payload.Data.IdleRemainingMS)
	}
	if payload.Data.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("expires_at = %d, want future absolute expiry", payload.Data.ExpiresAt)
	}
}
