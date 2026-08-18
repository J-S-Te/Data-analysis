package alertworker

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type memoryStore struct {
	rules         []Rule
	candidates    []ContractExpiryCandidate
	alerts        map[string]Alert
	requestedDays int
	findCalls     int
}

func (s *memoryStore) ListEnabledRules(context.Context) ([]Rule, error) {
	return s.rules, nil
}

func (s *memoryStore) FindContractExpiryCandidates(
	_ context.Context,
	_ time.Time,
	days int,
) ([]ContractExpiryCandidate, error) {
	s.findCalls++
	s.requestedDays = days
	return s.candidates, nil
}

func (s *memoryStore) UpsertAlerts(_ context.Context, alerts []Alert) error {
	if s.alerts == nil {
		s.alerts = make(map[string]Alert)
	}
	for _, alert := range alerts {
		key := alert.TenantID + "/" + alert.AlertType + "/" + alert.TargetRef
		s.alerts[key] = alert
	}
	return nil
}

func TestRunOnceUpsertsContractExpiryAlertsIdempotently(t *testing.T) {
	dueDate := time.Date(2026, time.September, 8, 0, 0, 0, 0, time.UTC)
	store := &memoryStore{
		rules: []Rule{{Code: RuleContractExpiry, Severity: "high", ThresholdJSON: []byte(`{"days": 21}`)}},
		candidates: []ContractExpiryCandidate{{
			TenantID:  "tenant-1",
			TargetRef: "contract-1",
			Title:     "合同即将到期",
			DueDate:   dueDate,
		}},
	}
	worker := New(store)
	worker.now = func() time.Time { return time.Date(2026, time.August, 17, 8, 0, 0, 0, time.UTC) }

	for run := 0; run < 2; run++ {
		count, err := worker.RunOnce(context.Background())
		if err != nil {
			t.Fatalf("RunOnce() error = %v", err)
		}
		if count != 1 {
			t.Fatalf("RunOnce() count = %d, want 1", count)
		}
	}
	if store.requestedDays != 21 {
		t.Fatalf("requested days = %d, want 21", store.requestedDays)
	}
	if len(store.alerts) != 1 {
		t.Fatalf("idempotent alert count = %d, want 1", len(store.alerts))
	}
	for _, alert := range store.alerts {
		if len(alert.ID) != 26 {
			t.Errorf("alert ID length = %d, want 26", len(alert.ID))
		}
		if alert.Severity != "HIGH" {
			t.Errorf("severity = %q, want HIGH", alert.Severity)
		}
		if !alert.DueDate.Equal(dueDate) {
			t.Errorf("due date = %v, want %v", alert.DueDate, dueDate)
		}
	}
}

func TestRunOnceRejectsInvalidThresholdAndContinuesOtherRules(t *testing.T) {
	store := &memoryStore{rules: []Rule{
		{Code: RuleContractExpiry, ThresholdJSON: []byte(`{"days": 0}`)},
		{Code: "PROJECT_DELAY"},
	}}
	worker := New(store)

	count, err := worker.RunOnce(context.Background())
	if count != 0 {
		t.Fatalf("RunOnce() count = %d, want 0", count)
	}
	if err == nil {
		t.Fatal("RunOnce() error = nil, want joined configuration errors")
	}
	for _, want := range []string{"days must be between", "PROJECT_DELAY is not supported"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("RunOnce() error = %q, want substring %q", err, want)
		}
	}
	if store.findCalls != 0 {
		t.Fatalf("candidate query calls = %d, want 0", store.findCalls)
	}
}

func TestRunLoopRejectsNonPositiveInterval(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := RunLoop(context.Background(), 0, New(&memoryStore{}), logger)
	if err == nil {
		t.Fatal("RunLoop() error = nil, want invalid interval error")
	}
}
