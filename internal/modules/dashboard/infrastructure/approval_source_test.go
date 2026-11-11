package infrastructure

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/unified-identity-auth-platform/data-analysis/internal/aggregation"
)

// approvalFixture 在 scopedContractFixture 上补齐审批投影表与 last_success_at 列。
// 参数：t 为测试上下文。
// 返回值：与生产列名一致的 sqlite 连接。
func approvalFixture(t *testing.T) *gorm.DB {
	t.Helper()
	db := scopedContractFixture(t)
	for _, ddl := range []string{
		`CREATE TABLE dim_contract_approval (
			tenant_id TEXT, contract_id TEXT, approval_id TEXT, kind TEXT, status TEXT,
			started_at DATETIME, completed_at DATETIME, updated_at DATETIME,
			PRIMARY KEY (tenant_id, contract_id, approval_id))`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// insertApproval 写入一条审批投影行。
// 参数：db 为夹具连接，tenant/contract/approval 为标识，kind/status 为源词表值。
// 返回值：无，写入失败终止用例。
func insertApproval(t *testing.T, db *gorm.DB, tenant, contract, approval, kind, status string) {
	t.Helper()
	now := time.Now().UTC()
	if err := db.Exec(`INSERT INTO dim_contract_approval(tenant_id,contract_id,approval_id,kind,status,started_at,completed_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		tenant, contract, approval, kind, status, now, nil, now).Error; err != nil {
		t.Fatal(err)
	}
}

// markApprovalChannel 写入 contract_approval 的同步状态证据。
// 参数：db 为夹具连接，tenant 为租户，status 为 last_status，lastSuccess 为 last_success_at（nil 表示从未成功）。
// 返回值：无，写入失败终止用例。
func markApprovalChannel(t *testing.T, db *gorm.DB, tenant, status string, lastSuccess *time.Time) {
	t.Helper()
	now := time.Now().UTC()
	if err := db.Exec(`INSERT INTO sync_source(id,tenant_id,subsystem_code,db_schema,enabled,last_run_at,last_status,last_success_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?)`,
		"src-"+tenant, tenant, aggregation.ContractApprovalSource, aggregation.ContractApprovalSource,
		true, now, status, lastSuccess, now).Error; err != nil {
		t.Fatal(err)
	}
}

// TestParseApprovalSource 验证 DASHBOARD_APPROVAL_SOURCE 的默认值与非法值处理。
// 参数：t 为测试上下文。
// 返回值：无；非法值必须报错，由启动装配失败关闭。
func TestParseApprovalSource(t *testing.T) {
	cases := []struct {
		raw     string
		want    ApprovalSource
		wantErr bool
	}{
		{raw: "", want: ApprovalSourceStatus},
		{raw: "  ", want: ApprovalSourceStatus},
		{raw: "status", want: ApprovalSourceStatus},
		{raw: "INSTANCES", want: ApprovalSourceInstances},
		{raw: " instances ", want: ApprovalSourceInstances},
		{raw: "pending", wantErr: true},
	}
	for _, tc := range cases {
		got, err := ParseApprovalSource(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("ParseApprovalSource(%q) error = nil, want rejection", tc.raw)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("ParseApprovalSource(%q) = %q/%v, want %q", tc.raw, got, err, tc.want)
		}
	}
}

// TestScopedContractInstancesCountsRunningApprovals 验证 instances 口径：
// 只按 dim_contract_approval.status='running' 计数（含 kind=status_change 的变更审批），
// pending 无在途实例不计入、已结束实例不计入、跨租户实例不计入；
// 默认 status 口径保持“源状态 pending”不变。
// 参数：t 为测试上下文。
// 返回值：无。
func TestScopedContractInstancesCountsRunningApprovals(t *testing.T) {
	db := approvalFixture(t)
	const tenant, org = "tenant-approval", "org-1"
	now := time.Now().In(scopedContractBusinessLocation(t))
	today := now.Format("2006-01-02")

	// 变更审批中的合同：源状态仍是 active，旧口径会漏计。
	insertScopedContract(t, db, "NORMAL", tenant, "c-active-approving", "active", today, "u1", org)
	insertApproval(t, db, tenant, "c-active-approving", "appr-change", "status_change", "running")
	// pending 但没有任何在途实例：instances 口径不得计入。
	insertScopedContract(t, db, "NORMAL", tenant, "c-pending-stale", "pending", "", "u1", org)
	// pending 且有在途实例：两种口径都计入。
	insertScopedContract(t, db, "NORMAL", tenant, "c-pending-running", "pending", "", "u1", org)
	insertApproval(t, db, tenant, "c-pending-running", "appr-new", "contract_approval", "running")
	// 已结束的审批实例不得计入。
	insertScopedContract(t, db, "NORMAL", tenant, "c-approved", "approved", "", "u1", org)
	insertApproval(t, db, tenant, "c-approved", "appr-done", "contract_approval", "approved")
	// 跨租户 running 实例不得影响本租户口径（同 contract_id 也不行）。
	insertApproval(t, db, "tenant-other", "c-active-approving", "appr-other", "status_change", "running")

	markApprovalChannel(t, db, tenant, "SUCCESS", nil)
	location := scopedContractBusinessLocation(t)
	ctx := context.Background()
	scope := queryScope{mode: "ORG", tenantID: tenant, orgUnitIDs: []string{org}}

	instances := NewGORMRepositoryWithApprovalSource(db, location, ApprovalSourceInstances)
	snapshot, available, err := instances.scopedContract(ctx, scope)
	if err != nil || !available {
		t.Fatalf("instances scoped contract read failed: available=%v err=%v", available, err)
	}
	if snapshot.ApprovalContracts != 2 {
		t.Fatalf("instances approval contracts = %d, want 2 (active 变更审批 + pending 在途)", snapshot.ApprovalContracts)
	}
	if snapshot.TotalContracts != 4 {
		t.Fatalf("total contracts = %d, want 4", snapshot.TotalContracts)
	}

	// 默认 status 口径不变：只数源状态 pending，忽略审批投影表。
	statusRepo := NewGORMRepositoryWithLocation(db, location)
	statusSnapshot, available, err := statusRepo.scopedContract(ctx, scope)
	if err != nil || !available {
		t.Fatalf("status scoped contract read failed: available=%v err=%v", available, err)
	}
	if statusSnapshot.ApprovalContracts != 2 {
		t.Fatalf("status approval contracts = %d, want 2 (two pending rows)", statusSnapshot.ApprovalContracts)
	}
}

// TestScopedContractInstancesRejectsWhenChannelNeverSucceeded 验证升级门禁：
// instances 口径下 contract_approval 通道从未成功（无行 / 只有 FAILED 且无 last_success_at）
// 时必须返回 ErrApprovalSourceNotReady，而不是静默返回 0。
// 参数：t 为测试上下文。
// 返回值：无。
func TestScopedContractInstancesRejectsWhenChannelNeverSucceeded(t *testing.T) {
	db := approvalFixture(t)
	const tenant, org = "tenant-approval-gate", "org-1"
	insertScopedContract(t, db, "NORMAL", tenant, "c-active-approving", "active", "", "u1", org)
	insertApproval(t, db, tenant, "c-active-approving", "appr-change", "status_change", "running")
	location := scopedContractBusinessLocation(t)
	ctx := context.Background()
	scope := queryScope{mode: "ORG", tenantID: tenant, orgUnitIDs: []string{org}}
	repository := NewGORMRepositoryWithApprovalSource(db, location, ApprovalSourceInstances)

	if _, available, err := repository.scopedContract(ctx, scope); !errors.Is(err, ErrApprovalSourceNotReady) || available {
		t.Fatalf("missing channel row: available=%v err=%v, want ErrApprovalSourceNotReady", available, err)
	}
	// 启动期硬校验同样拒绝。
	if err := ValidateApprovalSource(ctx, db, tenant, ApprovalSourceInstances); !errors.Is(err, ErrApprovalSourceNotReady) {
		t.Fatalf("ValidateApprovalSource() error = %v, want ErrApprovalSourceNotReady", err)
	}

	// 最新一轮失败但曾经成功过：允许切换（首次成功的证据不被后续失败清空）。
	succeededAt := time.Now().UTC().Add(-time.Hour)
	markApprovalChannel(t, db, tenant, "FAILED", &succeededAt)
	if err := ValidateApprovalSource(ctx, db, tenant, ApprovalSourceInstances); err != nil {
		t.Fatalf("ValidateApprovalSource() error = %v, want nil after first success", err)
	}
	snapshot, available, err := repository.scopedContract(ctx, scope)
	if err != nil || !available {
		t.Fatalf("instances read after first success: available=%v err=%v", available, err)
	}
	if snapshot.ApprovalContracts != 1 {
		t.Fatalf("approval contracts = %d, want 1", snapshot.ApprovalContracts)
	}

	// status 口径不需要通道证据（默认行为不变）。
	if err := ValidateApprovalSource(ctx, db, "tenant-without-channel", ApprovalSourceStatus); err != nil {
		t.Fatalf("status source must not require channel evidence: %v", err)
	}
}

// TestApprovalInstancesReadyRequiresSuccessEvidence 验证“曾经成功”的判据：
// last_status=SUCCESS 或 last_success_at 非空；PENDING/RUNNING/FAILED 且无历史成功不算。
// 参数：t 为测试上下文。
// 返回值：无。
func TestApprovalInstancesReadyRequiresSuccessEvidence(t *testing.T) {
	cases := []struct {
		name        string
		status      string
		lastSuccess *time.Time
		want        bool
	}{
		{name: "pending", status: "PENDING", want: false},
		{name: "running", status: "RUNNING", want: false},
		{name: "failed without history", status: "FAILED", want: false},
		{name: "success current", status: "SUCCESS", want: true},
		{name: "failed after success", status: "FAILED", lastSuccess: ptrTime(time.Now().UTC()), want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := approvalFixture(t)
			const tenant = "tenant-ready"
			markApprovalChannel(t, db, tenant, tc.status, tc.lastSuccess)
			ready, err := ApprovalInstancesReady(context.Background(), db, tenant)
			if err != nil {
				t.Fatal(err)
			}
			if ready != tc.want {
				t.Fatalf("ApprovalInstancesReady() = %v, want %v", ready, tc.want)
			}
		})
	}
}

func ptrTime(value time.Time) *time.Time { return &value }
