package application

import (
	"context"
	"errors"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

// ErrBusinessScopeUnavailable 防止仅有租户维度的物化结果
// 被展示为更窄组织或团队数据范围的统计。
var ErrBusinessScopeUnavailable = errors.New("business dashboard requires an explicit tenant data scope")

type businessRepository interface {
	LatestBusiness(context.Context, string, string) (domain.BusinessSnapshot, error)
}

func (service *Service) Business(ctx context.Context, tenant, section string) (domain.BusinessSnapshot, error) {
	if tenant == "" || (section != "report" && section != "finance") {
		return domain.BusinessSnapshot{}, errors.New("invalid business dashboard scope")
	}
	// 这些快照不含组织或团队维度。查询租户行后
	// 再筛选合计，无法恢复范围限定的结果。
	// 仅具备明确匹配 TENANT 授权的已验证主体可读取；
	// 角色、空范围及内部调用方不能隐含租户全范围访问。
	principal, authenticated := auth.FromContext(ctx)
	if !authenticated || principal.TenantID != tenant || principal.ScopeMode != "TENANT" {
		return domain.BusinessSnapshot{}, ErrBusinessScopeUnavailable
	}
	repo, ok := service.snapshots.(businessRepository)
	if !ok {
		return domain.BusinessSnapshot{}, errors.New("business statistics repository unavailable")
	}
	return repo.LatestBusiness(ctx, tenant, section)
}
