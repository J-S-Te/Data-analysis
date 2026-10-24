package alertworker

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAlertExpiryBeforeUpsertStopsNewAlertAndCanResume(t *testing.T) {
	s := &memoryStore{rules: []Rule{{Code: RuleContractExpiry}}, candidates: []ContractExpiryCandidate{{TenantID: "tenant", TargetRef: "contract", DueDate: time.Now()}}}
	w := New(s)
	checks := 0
	denied := errors.New("expired")
	w.LicenseCheck = func(context.Context) error {
		checks++
		if checks >= 3 {
			return denied
		}
		return nil
	}
	if _, err := w.RunOnce(context.Background()); !errors.Is(err, denied) || len(s.alerts) != 0 {
		t.Fatal("expired pass wrote alerts", err)
	}
	w.LicenseCheck = func(context.Context) error { return nil }
	if n, err := w.RunOnce(context.Background()); err != nil || n != 1 || len(s.alerts) != 1 {
		t.Fatal("renewal did not resume", n, err)
	}
}

func TestDeniedAlertWorkerDoesNotReadOrWriteStore(t *testing.T) {
	denied := errors.New("expired")
	w := New(nil)
	w.LicenseCheck = func(context.Context) error { return denied }
	if n, err := w.RunOnce(context.Background()); n != 0 || !errors.Is(err, denied) {
		t.Fatal(n, err)
	}
}
