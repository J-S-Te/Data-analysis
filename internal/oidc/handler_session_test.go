package oidc

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

type sessionEnvelope struct {
	Code string `json:"code"`
	Data struct {
		UserID             string `json:"user_id"`
		TenantID           string `json:"tenant_id"`
		ExpiresAt          int64  `json:"expires_at"`
		IdleTimeoutSeconds int64  `json:"idle_timeout_seconds"`
		IdleRemainingMS    int64  `json:"idle_remaining_ms"`
	} `json:"data"`
}

// TestAuthMeAppendsIdleFields 验证 /auth/me 追加空闲字段且既有字段与信封不变。
func TestAuthMeAppendsIdleFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service, _ := newSessionIdleTestService(t, 30*time.Minute, 8*time.Hour)
	handler := NewHandler(service, HTTPOptions{CookieName: "data_analysis_session"})
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodGet, "/data_analysis/api/v1/auth/me", nil)
	principal := auth.Principal{
		UserID: "user-1", TenantID: "tenant-1", DisplayName: "User One", Username: "user.one",
		Roles: []string{"sales"}, Permissions: map[string]struct{}{"dashboard.overview.view": {}},
		RoleConfigHash: "hash-1", AuthzRevision: 3, ExpiresAt: 1893456000,
		IdleTimeoutSeconds: 1800, IdleRemainingMS: 600000,
	}
	context.Request = request.WithContext(auth.WithPrincipal(request.Context(), principal))
	handler.AuthMe(context)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}
	var payload sessionEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Code != "OK" {
		t.Fatalf("code = %q, want OK", payload.Code)
	}
	if payload.Data.UserID != "user-1" || payload.Data.TenantID != "tenant-1" || payload.Data.ExpiresAt != 1893456000 {
		t.Fatalf("existing /auth/me fields changed: %+v", payload.Data)
	}
	if payload.Data.IdleTimeoutSeconds != 1800 || payload.Data.IdleRemainingMS != 600000 {
		t.Fatalf("idle fields = %d/%d, want 1800/600000", payload.Data.IdleTimeoutSeconds, payload.Data.IdleRemainingMS)
	}
}

// TestActivityRenewsIdleSession 验证活动接口返回统一信封并推进 last_interactive_at。
func TestActivityRenewsIdleSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service, db := newSessionIdleTestService(t, 30*time.Minute, 8*time.Hour)
	lastInteractive := time.Now().UTC().Add(-20 * time.Minute)
	seedIdleTestSession(t, db, func(session *Session) {
		session.LastInteractiveAt = &lastInteractive
		session.CreatedAt = lastInteractive
	})
	handler := NewHandler(service, HTTPOptions{CookieName: "data_analysis_session"})
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodPost, "/data_analysis/api/v1/auth/activity", nil)
	request.AddCookie(&http.Cookie{Name: "data_analysis_session", Value: idleTestRawToken})
	context.Request = request
	handler.Activity(context)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}
	var payload sessionEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Code != "OK" || payload.Data.IdleTimeoutSeconds != 1800 || payload.Data.IdleRemainingMS <= 0 {
		t.Fatalf("unexpected activity response: %+v", payload)
	}
	after := loadIdleTestSession(t, db)
	if after.LastInteractiveAt == nil || !after.LastInteractiveAt.After(lastInteractive) {
		t.Fatalf("activity did not advance last_interactive_at: %v", after.LastInteractiveAt)
	}
}

// TestActivityRejectsInactiveSession 验证已失效会话返回既有 401 错误格式且不复活。
func TestActivityRejectsInactiveSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service, db := newSessionIdleTestService(t, 30*time.Minute, 8*time.Hour)
	idleExpiredAt := time.Now().UTC().Add(-31 * time.Minute)
	seedIdleTestSession(t, db, func(session *Session) {
		session.LastInteractiveAt = &idleExpiredAt
		session.CreatedAt = idleExpiredAt
	})
	handler := NewHandler(service, HTTPOptions{CookieName: "data_analysis_session"})
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodPost, "/data_analysis/api/v1/auth/activity", nil)
	request.AddCookie(&http.Cookie{Name: "data_analysis_session", Value: idleTestRawToken})
	context.Request = request
	handler.Activity(context)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
	var payload sessionEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Code != "UNAUTHENTICATED" {
		t.Fatalf("code = %q, want UNAUTHENTICATED", payload.Code)
	}
	after := loadIdleTestSession(t, db)
	if after.LastInteractiveAt == nil || !after.LastInteractiveAt.Equal(idleExpiredAt) {
		t.Fatalf("inactive session was revived: %v", after.LastInteractiveAt)
	}
}

// TestActivityRequiresCookie 验证缺少会话 cookie 时返回 401。
func TestActivityRequiresCookie(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service, _ := newSessionIdleTestService(t, 30*time.Minute, 8*time.Hour)
	handler := NewHandler(service, HTTPOptions{CookieName: "data_analysis_session"})
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodPost, "/data_analysis/api/v1/auth/activity", nil)
	handler.Activity(context)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
}
