package http

import (
	"context"
	"encoding/json"
	"errors"
	stdhttp "net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

type dashboardQueryServiceStub struct{ projectErr error }

func (stub dashboardQueryServiceStub) Contract(context.Context, string) (domain.ContractSnapshot, error) {
	return domain.ContractSnapshot{}, nil
}

func (stub dashboardQueryServiceStub) Project(context.Context, string) (domain.ProjectSnapshot, error) {
	return domain.ProjectSnapshot{}, stub.projectErr
}

func TestProjectPreservesInvalidSnapshotErrorCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	request := httptest.NewRequest(stdhttp.MethodGet, "/dashboard/project", nil)
	request = request.WithContext(auth.WithPrincipal(request.Context(), auth.Principal{TenantID: "tenant-1"}))
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = request

	NewHandler(dashboardQueryServiceStub{projectErr: errors.Join(application.ErrInvalidProjectSnapshot, errors.New("invalid JSON"))}).Project(context)

	if response.Code != stdhttp.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "PROJECT_DASHBOARD_INVALID" {
		t.Fatalf("code = %q, want PROJECT_DASHBOARD_INVALID", body.Code)
	}
}
