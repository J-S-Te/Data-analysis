package alertworker

import (
	"context"
	"errors"
	"testing"
)

type expiryDuringCountStore struct {
	*memoryStore
	expire func()
}

// CountActiveAlerts 在真实业务读取边界返回后模拟许可到期。
// 参数：ctx 为上下文；ruleCode 为被评估的规则。
// 返回值：既有夹具中的活动数量和读取错误。
func (s *expiryDuringCountStore) CountActiveAlerts(ctx context.Context, ruleCode string) (int64, error) {
	count, err := s.memoryStore.CountActiveAlerts(ctx, ruleCode)
	s.expire()
	return count, err
}

// TestReconcileRechecksLicenseAfterCounting 验证许可在查询期间到期后不再写收敛状态。
// 参数：t 为隔离测试上下文，存储替身只观察业务写入边界。
// 返回值：无；许可错误被吞掉或仍然执行收敛时失败。
func TestReconcileRechecksLicenseAfterCounting(t *testing.T) {
	active := true
	denied := errors.New("test license expired during count")
	store := &expiryDuringCountStore{memoryStore: &memoryStore{
		rules:       []Rule{{Code: RuleContractExpiry, Severity: "HIGH"}},
		candidates:  []ContractExpiryCandidate{{TenantID: "tenant-1", TargetRef: "contract-1"}},
		activeCount: 1,
	}, expire: func() { active = false }}
	worker := New(store)
	worker.SetLifecycle(LifecycleOptions{AutoResolve: true})
	worker.LicenseCheck = func(context.Context) error {
		if !active {
			return denied
		}
		return nil
	}
	_, err := worker.RunOnce(context.Background())
	if !errors.Is(err, denied) {
		t.Fatalf("license expiry was not reported: %v", err)
	}
	if store.reconcile != nil || len(store.order) != 2 ||
		store.order[0] != "upsert" || store.order[1] != "count" {
		t.Fatalf("business reconciliation ran after expiry: %v", store.order)
	}
}
