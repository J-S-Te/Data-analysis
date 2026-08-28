package application

import (
	"context"
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/admin/domain"
)

type repositoryStub struct {
	source        domain.SyncSource
	found         bool
	createJob     bool
	jobs          []domain.SyncJob
	rules         []domain.AlertRule
	defaultTenant string
}

func (stub *repositoryStub) ListSources(context.Context, string) ([]domain.SyncSource, error) {
	return nil, nil
}
func (stub *repositoryStub) FindSource(context.Context, string, string) (domain.SyncSource, bool, error) {
	return stub.source, stub.found, nil
}
func (stub *repositoryStub) CreateSyncJob(_ context.Context, job domain.SyncJob) (bool, error) {
	if !stub.createJob {
		return false, nil
	}
	stub.jobs = append(stub.jobs, job)
	return true, nil
}
func (stub *repositoryStub) EnsureDefaultAlertRules(_ context.Context, tenantID string, _ time.Time) error {
	stub.defaultTenant = tenantID
	return nil
}
func (stub *repositoryStub) ListAlertRules(context.Context, string) ([]domain.AlertRule, error) {
	return nil, nil
}
func (stub *repositoryStub) ReplaceAlertRules(_ context.Context, _ string, rules []domain.AlertRule) error {
	stub.rules = rules
	return nil
}

func TestTriggerSourceQueuesEnabledSource(t *testing.T) {
	stub := &repositoryStub{source: domain.SyncSource{ID: "source-1", SubsystemCode: "contract_management", Enabled: true}, found: true, createJob: true}
	now := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	service := &Service{repository: stub, now: func() time.Time { return now }, newID: func() string { return "job-1" }}
	job, err := service.TriggerSource(context.Background(), "tenant-1", "source-1")
	if err != nil || job.Status != "QUEUED" || job.RequestedAt != now || len(stub.jobs) != 1 {
		t.Fatalf("unexpected queued job=%#v jobs=%#v err=%v", job, stub.jobs, err)
	}
}

func TestTriggerSourceRejectsDuplicateActiveJob(t *testing.T) {
	stub := &repositoryStub{source: domain.SyncSource{ID: "source-1", Enabled: true}, found: true}
	service := NewService(stub)
	if _, err := service.TriggerSource(context.Background(), "tenant-1", "source-1"); err != ErrSyncAlreadyQueued {
		t.Fatalf("error = %v, want %v", err, ErrSyncAlreadyQueued)
	}
	if len(stub.jobs) != 0 {
		t.Fatalf("duplicate request appended %d job(s)", len(stub.jobs))
	}
}

func TestReplaceAlertRulesRejectsMissingRequiredFields(t *testing.T) {
	service := NewService(&repositoryStub{})
	if _, err := service.ReplaceAlertRules(context.Background(), "tenant-1", "operator-1", []domain.AlertRule{{RuleCode: "rule-1"}}); err != ErrRulePayloadInvalid {
		t.Fatalf("error = %v, want %v", err, ErrRulePayloadInvalid)
	}
}

func TestReplaceAlertRulesRejectsUnsafeThresholdAndDuplicateCode(t *testing.T) {
	service := NewService(&repositoryStub{})
	invalidThreshold := `{"days":0}`
	validThreshold := `{"days":30}`
	valid := domain.AlertRule{RuleCode: "CONTRACT_EXPIRY", Name: "合同到期提醒", SourceFCT: "dim_contract", Severity: "HIGH", ThresholdJSON: &validThreshold}
	invalid := valid
	invalid.ThresholdJSON = &invalidThreshold
	if _, err := service.ReplaceAlertRules(context.Background(), "tenant-1", "operator-1", []domain.AlertRule{invalid}); err != ErrRulePayloadInvalid {
		t.Fatalf("invalid threshold error = %v, want %v", err, ErrRulePayloadInvalid)
	}
	if _, err := service.ReplaceAlertRules(context.Background(), "tenant-1", "operator-1", []domain.AlertRule{valid, valid}); err != ErrRulePayloadInvalid {
		t.Fatalf("duplicate rule error = %v, want %v", err, ErrRulePayloadInvalid)
	}
}

func TestListAlertRulesInitializesTenantDefaults(t *testing.T) {
	stub := &repositoryStub{}
	service := NewService(stub)
	if _, err := service.ListAlertRules(context.Background(), "tenant-1"); err != nil {
		t.Fatalf("ListAlertRules() error = %v", err)
	}
	if stub.defaultTenant != "tenant-1" {
		t.Fatalf("default tenant = %q, want tenant-1", stub.defaultTenant)
	}
}
