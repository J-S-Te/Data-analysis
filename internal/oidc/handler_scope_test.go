package oidc

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

// TestAuthMeReturnsTrustedDataScope 验证页面范围来自可信主体而不是角色名或查询参数。
// 参数：t 为仅使用内存HTTP上下文的测试环境。
// 返回值：无；范围丢失、扩大或空集合被省略时测试失败。
func TestAuthMeReturnsTrustedDataScope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{"ORG", "NONE"} {
		t.Run(mode, func(t *testing.T) {
			p := auth.Principal{TenantID: "t", UserID: "u", Roles: []string{"dashboard_admin"},
				ScopeMode: mode, OrganizationIDs: []string{"org-a"}, TeamIDs: []string{}}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			r := httptest.NewRequest("GET", "/auth/me?scope_mode=TENANT", nil)
			c.Request = r.WithContext(auth.WithPrincipal(r.Context(), p))
			NewHandler(nil, HTTPOptions{}).AuthMe(c)
			var body struct {
				Data struct {
					Mode  string   `json:"scope_mode"`
					Orgs  []string `json:"organization_ids"`
					Teams []string `json:"team_ids"`
				} `json:"data"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Data.Mode != mode || !reflect.DeepEqual(body.Data.Orgs, p.OrganizationIDs) ||
				body.Data.Teams == nil || len(body.Data.Teams) != 0 {
				t.Fatalf("可信范围未完整保留: %+v", body.Data)
			}
		})
	}
}
