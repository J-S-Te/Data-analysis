package oidc

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

// TestAuthMeReportsEmbedCapability 验证会话明确携带嵌入启用能力且缺主体仍拒绝。
// 参数：t 为测试上下文。
// 返回值：无。
func TestAuthMeReportsEmbedCapability(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(nil, HTTPOptions{})
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest("GET", "/auth/me", nil)
	c.Request = req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{UserID: "u", TenantID: "t"}))
	handler.AuthMe(c)
	var result struct {
		Data struct {
			Capabilities map[string]bool `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if enabled, exists := result.Data.Capabilities["dashboard_embed_enabled"]; !exists || !enabled {
		t.Fatalf("legacy default capability missing: %s", recorder.Body.String())
	}
	disabled := false
	handler = NewHandler(nil, HTTPOptions{DashboardEmbedEnabled: &disabled})
	recorder = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(recorder)
	c.Request = req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{UserID: "u", TenantID: "t"}))
	handler.AuthMe(c)
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if enabled, exists := result.Data.Capabilities["dashboard_embed_enabled"]; !exists || enabled {
		t.Fatalf("disabled capability incorrect: %s", recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(recorder)
	c.Request = req
	handler.AuthMe(c)
	if recorder.Code != 401 {
		t.Fatalf("unauthenticated status=%d", recorder.Code)
	}
}
