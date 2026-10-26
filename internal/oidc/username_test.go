package oidc

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

func TestAuthMeReturnsLoginUsernameSeparatelyFromDisplayName(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("GET", "/api/v1/auth/me", nil)
	c.Request = c.Request.WithContext(auth.WithPrincipal(context.Background(), auth.Principal{UserID: "internal", Username: "alice", DisplayName: "张三"}))
	(&Handler{}).AuthMe(c)
	var body struct {
		Data struct {
			Username    string `json:"username"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != 200 || body.Data.Username != "alice" || body.Data.DisplayName != "张三" {
		t.Fatalf("response=%s", recorder.Body.String())
	}
}
