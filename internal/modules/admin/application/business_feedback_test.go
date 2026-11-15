package application

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/admin/domain"
)

type savedReadFailureRepository struct {
	Repository
	saved []domain.AlertRule
	cause error
}

// ReplaceAlertRules 用途：模拟已确认提交的存储边界，不替换应用层完成分类逻辑。
// 参数：ctx 为请求上下文，tenant 为可信租户，rules 为应用已校验规则。
// 返回值：保存确认成功。
func (r *savedReadFailureRepository) ReplaceAlertRules(_ context.Context, _ string, rules []domain.AlertRule) error {
	r.saved = rules
	return nil
}

// ListAlertRules 用途：模拟写入完成后的独立读取故障。
// 参数：ctx 为请求上下文，tenant 为可信租户。
// 返回值：明确的读取失败，不撤销先前写入。
func (r *savedReadFailureRepository) ListAlertRules(context.Context, string) ([]domain.AlertRule, error) {
	return nil, r.cause
}

// TestRuleFeedbackDistinguishesSavedReadFailure 用途：防止已确认规则写入被后续读取故障改报未保存。
// 参数：t 为只替换持久化边界的应用测试上下文。
// 返回值：无；完成事实丢失或底层原因链断开时失败。
func TestRuleFeedbackDistinguishesSavedReadFailure(t *testing.T) {
	cause := errors.New("read unavailable")
	repository := &savedReadFailureRepository{cause: cause}
	threshold := `{"days":30}`
	_, err := NewService(repository).ReplaceAlertRules(context.Background(), "tenant", "actor", []domain.AlertRule{
		{RuleCode: "CONTRACT_EXPIRY", Name: "到期提醒", SourceFCT: "dim_contract", Severity: "HIGH", ThresholdJSON: &threshold},
	})
	if len(repository.saved) != 1 || !errors.Is(err, cause) || !strings.Contains(err.Error(), "已保存") {
		t.Fatalf("保存事实或诊断链丢失: saved=%d err=%v", len(repository.saved), err)
	}
}
