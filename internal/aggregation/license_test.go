package aggregation

import (
	"context"
	"errors"
	"testing"
)

func TestPassExpiresBetweenTasksAndResumesAfterRenewal(t *testing.T) {
	allowed := true
	calls := 0
	denied := errors.New("expired")
	p := Pass{LicenseCheck: func(context.Context) error {
		if !allowed {
			return denied
		}
		return nil
	}, SyncSources: func(context.Context) error { calls++; allowed = false; return nil }, SyncJobs: func(context.Context) error { calls++; return nil }}
	if !errors.Is(p.RunOnce(context.Background()), denied) || calls != 1 {
		t.Fatal("work ran after expiry")
	}
	allowed = true
	p.SyncSources = nil
	if err := p.RunOnce(context.Background()); err != nil || calls != 2 {
		t.Fatal("renewal did not resume", err, calls)
	}
}

func TestDeniedPassDoesNotClaimJobsOrCallSources(t *testing.T) {
	denied := errors.New("expired")
	calls := 0
	p := Pass{LicenseCheck: func(context.Context) error { return denied }, SyncJobs: func(context.Context) error { calls++; return nil }, SyncContractDashboard: func(context.Context) error { calls++; return nil }}
	if !errors.Is(p.RunOnce(context.Background()), denied) || calls != 0 {
		t.Fatal("denied pass performed work")
	}
	r := &Runner{LicenseCheck: func(context.Context) error { return denied }}
	for _, run := range []func(context.Context) error{r.RunOnce, r.RunQueued, r.EnsureSyncSources, r.PrecomputeContractSigning} {
		if !errors.Is(run(context.Background()), denied) {
			t.Fatal("denied runner accessed database")
		}
	}
	a := NewAPISyncRunner(nil, APISyncOptions{LicenseCheck: func(context.Context) error { return denied }})
	if !errors.Is(a.SyncContractDashboard(context.Background()), denied) {
		t.Fatal("denied API runner performed request")
	}
	if !errors.Is(a.SyncProjectDashboard(context.Background()), denied) {
		t.Fatal("denied project runner performed request")
	}
}
