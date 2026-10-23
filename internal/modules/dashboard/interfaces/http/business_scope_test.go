package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/unified-identity-auth-platform/data-analysis/internal/middleware"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

type businessHTTPRepository struct{ reads int }

func (*businessHTTPRepository) LatestContract(context.Context, string) (domain.ContractSnapshot, bool, error) {
	return domain.ContractSnapshot{}, false, nil
}
func (*businessHTTPRepository) LatestProject(context.Context, string) (domain.ProjectSnapshot, bool, error) {
	return domain.ProjectSnapshot{}, false, nil
}
func (repo *businessHTTPRepository) LatestBusiness(_ context.Context, tenant, _ string) (domain.BusinessSnapshot, error) {
	repo.reads++
	return domain.BusinessSnapshot{Available: true, TenantID: tenant, Metrics: map[string]any{"tenant_total": 9137}}, nil
}

// 使用真实处理器和应用服务，仅替换 SQL 输入输出。
func TestBusinessHTTPScopeAndPermissionRemainIndependent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, section := range []string{"report", "finance"} {
		for _, tc := range []struct {
			scope      string
			permission bool
			status     int
			code       string
		}{
			{"TENANT", true, 200, "OK"},
			{"ORG", true, 403, "BUSINESS_DASHBOARD_SCOPE_UNAVAILABLE"},
			{"TEAM", true, 403, "BUSINESS_DASHBOARD_SCOPE_UNAVAILABLE"},
			{"NONE", true, 403, "BUSINESS_DASHBOARD_SCOPE_UNAVAILABLE"},
			{"", true, 403, "BUSINESS_DASHBOARD_SCOPE_UNAVAILABLE"},
			{"UNKNOWN", true, 403, "BUSINESS_DASHBOARD_SCOPE_UNAVAILABLE"},
			{"TENANT", false, 403, "FORBIDDEN"},
		} {
			t.Run(section+"/"+tc.scope+"/"+tc.code, func(t *testing.T) {
				repo := &businessHTTPRepository{}
				handler := NewHandler(application.NewService(repo))
				router := gin.New()
				permission := "dashboard." + section + ".view"
				router.GET("/dashboard/"+section, middleware.RequirePermission(permission), handler.Business(section))
				principal := auth.Principal{TenantID: "tenant-1", UserID: "director", ScopeMode: tc.scope, Roles: []string{"admin"}, OrganizationIDs: []string{"org-1"}, TeamIDs: []string{"team-1"}, Permissions: map[string]struct{}{}}
				if tc.permission {
					principal.Permissions[permission] = struct{}{}
				}
				request := httptest.NewRequest(http.MethodGet, "/dashboard/"+section, nil)
				request = request.WithContext(auth.WithPrincipal(request.Context(), principal))
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				var body struct {
					Code    string          `json:"code"`
					Message string          `json:"message"`
					Data    json.RawMessage `json:"data"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if response.Code != tc.status || body.Code != tc.code {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
				if tc.status == 200 {
					if repo.reads != 1 || !strings.Contains(string(body.Data), "9137") {
						t.Fatalf("authorized snapshot missing: %s reads=%d", response.Body.String(), repo.reads)
					}
				} else if repo.reads != 0 || len(body.Data) != 0 || strings.Contains(response.Body.String(), "9137") {
					t.Fatalf("denied request accessed statistics: %s reads=%d", response.Body.String(), repo.reads)
				}
				if tc.code == "BUSINESS_DASHBOARD_SCOPE_UNAVAILABLE" && !strings.Contains(body.Message, "范围") {
					t.Fatalf("scope denial is not understandable: %s", response.Body.String())
				}
			})
		}
	}
}

func TestBusinessHTTPWithoutSessionRemainsUnauthenticated(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &businessHTTPRepository{}
	router := gin.New()
	router.GET("/dashboard/report", NewHandler(application.NewService(repo)).Business("report"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/dashboard/report", nil))
	if w.Code != http.StatusUnauthorized || repo.reads != 0 {
		t.Fatalf("unauthenticated request read statistics: %d %s reads=%d", w.Code, w.Body.String(), repo.reads)
	}
}
