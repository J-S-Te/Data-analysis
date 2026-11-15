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

type advancedDashboardServiceStub struct {
	dashboardQueryServiceStub
	regions domain.ContractRegionDistribution
}

// Contracts 返回空合同明细以满足区域测试的服务契约。
// 参数：请求上下文、租户和分页参数仅用于接口匹配。
// 返回值：空列表、零条目及空错误。
func (stub advancedDashboardServiceStub) Contracts(context.Context, string, int, int) ([]domain.ContractDetail, int64, error) {
	return nil, 0, nil
}

// Trend 返回空趋势以满足区域测试的服务契约。
// 参数：请求上下文、租户和窗口月份仅用于接口匹配。
// 返回值：空趋势及空错误。
func (stub advancedDashboardServiceStub) Trend(context.Context, string, int) ([]domain.TrendPoint, error) {
	return nil, nil
}

// ContractRegions 返回预设的区域分布以隔离 HTTP 适配层。
// 参数：请求上下文和租户仅用于接口匹配。
// 返回值：预设区域分布及空错误。
func (stub advancedDashboardServiceStub) ContractRegions(context.Context, string) (domain.ContractRegionDistribution, error) {
	return stub.regions, nil
}

// TestContractRegionsReturnsAvailabilityEnvelope 验证区域分布保留不可用原因。
// 参数：t 为仅使用内存响应记录器的 HTTP 测试上下文。
// 返回值：无；响应未保留空列表或可用性信封时失败。
func TestContractRegionsReturnsAvailabilityEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	request := httptest.NewRequest(stdhttp.MethodGet, "/dashboard/contract-regions", nil)
	request = request.WithContext(auth.WithPrincipal(request.Context(), auth.Principal{TenantID: "tenant-1"}))
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = request

	NewHandler(advancedDashboardServiceStub{regions: domain.ContractRegionDistribution{
		Available: false,
		Items:     []domain.ContractRegionTotal{},
		Basis:     "区域分布只支持全租户范围",
	}}).ContractRegions(context)

	if recorder.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Data domain.ContractRegionDistribution `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Data.Available || body.Data.Items == nil || len(body.Data.Items) != 0 || body.Data.Basis == "" {
		t.Fatalf("unexpected envelope: %#v", body.Data)
	}
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
