package aggregation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/J-S-Te/license-core"
	licruntime "github.com/J-S-Te/license-core/runtime"
	"path/filepath"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
)

func queueTestRunner(t *testing.T) (*Runner, SyncJob) {
	t.Helper()
	db := newScanTestDB(t)
	if err := db.AutoMigrate(&SyncJob{}, &businessRow{}, &syncRecoveryAudit{}); err != nil {
		t.Fatal(err)
	}
	active := "ACTIVE"
	job := SyncJob{TenantID: "tenant", ID: "job", SourceID: "source", SubsystemCode: "settlement", Status: "QUEUED", RequestedAt: time.Now().UTC(), ActiveKey: &active}
	if err := db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	return &Runner{aggDB: db, tenantID: "tenant", sources: map[string]string{}}, job
}

func TestInvalidQueueSettingsFailBeforeClaim(t *testing.T) {
	for _, r := range []*Runner{
		{QueueLeaseDuration: -time.Second},
		{QueueLeaseDuration: time.Second, QueueHeartbeatInterval: time.Second},
		{QueueTaskTimeout: -time.Second},
	} {
		if err := r.ValidateQueueSettings(); err == nil {
			t.Fatal("invalid queue configuration accepted")
		}
		if err := r.RunQueued(context.Background()); err == nil {
			t.Fatal("invalid queue configuration reached database")
		}
	}
}

func TestSignedLicenseExpiryPausesQueuedLeaseAndRenewalReplaysIdempotently(t *testing.T) {
	r, job := queueTestRunner(t)
	ctx := context.Background()
	pp, ps, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	vp, vs, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	binding := licruntime.Binding{InstanceID: "instance", Environment: "prod", Application: "data_analysis", ServiceID: "aggregation-worker"}
	local, err := licruntime.NewFileRuntime(filepath.Join(t.TempDir(), "license-state.json"), binding, map[string]ed25519.PublicKey{"platform": pp}, map[string]ed25519.PublicKey{"vendor": vp})
	if err != nil {
		t.Fatal(err)
	}
	const base int64 = 1800000000
	var clock atomic.Int64
	clock.Store(base + 9)
	apply := func(version uint64, start, end, at int64) {
		t.Helper()
		l := core.License{ProtocolVersion: core.ProtocolVersion, Issuer: core.Issuer, ID: "license", Version: version, CustomerID: "customer", ProductID: core.Product, Environment: binding.Environment, InstanceID: binding.InstanceID, IssuedAt: base - 100, NotBefore: start, Applications: []core.Application{{Code: binding.Application, Kind: "FULL", NotBefore: start, ExpiresAt: end}}}
		raw, e := core.Sign(l, "vendor", vs)
		if e != nil {
			t.Fatal(e)
		}
		s := licruntime.PlatformSnapshot{Protocol: licruntime.Protocol, InstanceID: binding.InstanceID, Environment: binding.Environment, Application: binding.Application, ServiceID: binding.ServiceID, Revision: version, EnforcementState: licruntime.Pending, MigrationEligible: true, CurrentLicenseJWS: raw, HighestLicenseVersion: version, SnapshotIssuedAt: at}
		snapshot, e := licruntime.SignSnapshot(s, "platform", ps)
		if e != nil {
			t.Fatal(e)
		}
		if e = local.ApplySnapshot(ctx, snapshot, time.Unix(at, 0)); e != nil {
			t.Fatal(e)
		}
	}
	apply(1, base-100, base+10, base+9)
	r.LicenseCheck = func(ctx context.Context) error {
		return local.Evaluate(ctx, core.MUTATE_BUSINESS, time.Unix(clock.Load(), 0))
	}
	l, claimed, err := r.claimQueueJob(ctx, job, time.Minute)
	if err != nil || !claimed {
		t.Fatal(claimed, err)
	}
	err = r.executeQueueJob(ctx, job, l, time.Minute, 10*time.Second, time.Minute, func(ctx context.Context, _ string, _ string) error {
		if e := r.storeBusiness(ctx, "finance", domain.BusinessSnapshot{Metrics: map[string]any{"pass": 1}}); e != nil {
			return e
		}
		clock.Store(base + 10)
		return r.checkLicense(ctx)
	})
	if !errors.Is(err, core.ErrDenied) {
		t.Fatalf("signed expiry did not pause: %v", err)
	}
	r.aggDB.First(&job, "id=?", "job")
	if job.Status != "QUEUED" || job.ActiveKey == nil || job.LeaseOwner != nil || job.LeaseGeneration != 1 {
		t.Fatalf("paused queue state: %+v", job)
	}
	if err = r.RunQueued(ctx); !errors.Is(err, core.ErrDenied) {
		t.Fatalf("expired replay allowed: %v", err)
	}
	clock.Store(base + 12)
	apply(2, base+11, base+100, base+12)
	l, claimed, err = r.claimQueueJob(ctx, job, time.Minute)
	if err != nil || !claimed {
		t.Fatal(claimed, err)
	}
	err = r.executeQueueJob(ctx, job, l, time.Minute, 10*time.Second, time.Minute, func(ctx context.Context, _ string, _ string) error {
		return r.storeBusiness(ctx, "finance", domain.BusinessSnapshot{Metrics: map[string]any{"pass": 2}})
	})
	if err != nil {
		t.Fatal(err)
	}
	var count int64
	if err = r.aggDB.Model(&businessRow{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("replay duplicated rows: %d %v", count, err)
	}
	r.aggDB.First(&job, "id=?", "job")
	if job.Status != "SUCCESS" || job.ActiveKey != nil || job.LeaseGeneration != 2 {
		t.Fatalf("renewed completion: %+v", job)
	}
}

func TestLegacyRecoveryAuditAndStoppedEvidenceRecheckRollback(t *testing.T) {
	r, job := queueTestRunner(t)
	ctx := context.Background()
	r.aggDB.Model(&SyncJob{}).Where("id=?", job.ID).Update("status", "RUNNING")
	calls := 0
	in := LegacyRecoveryInput{JobIDs: []string{job.ID}, Operator: "operator", VerifyStopped: func(context.Context) ([]byte, error) {
		calls++
		if calls == 2 {
			return nil, errors.New("worker restarted")
		}
		return []byte(`{"stopped":true}`), nil
	}}
	if err := r.RecoverLegacySyncJobs(ctx, in); err == nil {
		t.Fatal("worker restart did not abort recovery")
	}
	var got SyncJob
	r.aggDB.First(&got, "id=?", job.ID)
	if got.Status != "RUNNING" {
		t.Fatal("recovery committed despite changed stop evidence")
	}
	var count int64
	r.aggDB.Model(&syncRecoveryAudit{}).Count(&count)
	if count != 0 {
		t.Fatal("rolled-back recovery audit survived")
	}
	in.VerifyStopped = func(context.Context) ([]byte, error) { return []byte(`{"stopped":true}`), nil }
	if err := r.RecoverLegacySyncJobs(ctx, in); err != nil {
		t.Fatal(err)
	}
	var audit syncRecoveryAudit
	if err := r.aggDB.First(&audit).Error; err != nil {
		t.Fatal(err)
	}
	if audit.OperatorID != "operator" || audit.JobID != job.ID || len(audit.EvidenceSHA256) != 64 {
		t.Fatalf("missing recovery provenance: %+v", audit)
	}
}

func TestSyncQueueSingleOwnerConcurrencyAndTenantIsolation(t *testing.T) {
	r, job := queueTestRunner(t)
	var wg sync.WaitGroup
	successes := make(chan queueLease, 8)
	failures := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, claimed, err := r.claimQueueJob(context.Background(), job, time.Minute)
			if err != nil {
				failures <- err
			}
			if claimed {
				successes <- l
			}
		}()
	}
	wg.Wait()
	close(successes)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	if len(successes) != 1 {
		t.Fatalf("owners=%d", len(successes))
	}
	other := &Runner{aggDB: r.aggDB, tenantID: "other"}
	if _, claimed, err := other.claimQueueJob(context.Background(), job, time.Minute); claimed || err == nil {
		t.Fatalf("cross-tenant claim: %v %v", claimed, err)
	}
}

func TestExpiredOwnerCannotRenewFinalizeOrPublishAfterTakeover(t *testing.T) {
	r, job := queueTestRunner(t)
	ctx := context.Background()
	old, ok, err := r.claimQueueJob(ctx, job, time.Minute)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err = r.aggDB.Model(&SyncJob{}).Where("id=?", "job").Update("lease_expires_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	if err = r.renewQueueLease(ctx, old, time.Minute); !errors.Is(err, ErrSyncLeaseLost) {
		t.Fatalf("expired owner revived: %v", err)
	}
	if err = r.aggDB.First(&job, "id=?", "job").Error; err != nil {
		t.Fatal(err)
	}
	current, ok, err := r.claimQueueJob(ctx, job, time.Minute)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if current.generation != old.generation+1 || current.owner == old.owner {
		t.Fatal("takeover did not create independent owner/generation")
	}
	oldCtx := context.WithValue(ctx, queueLeaseKey{}, old)
	if err = r.storeBusiness(oldCtx, "finance", domain.BusinessSnapshot{Metrics: map[string]any{"old": true}}); !errors.Is(err, ErrSyncLeaseLost) {
		t.Fatalf("late publish accepted: %v", err)
	}
	if err = r.finishQueueJob(ctx, old, nil, false); !errors.Is(err, ErrSyncLeaseLost) {
		t.Fatalf("late finalize accepted: %v", err)
	}
	if err = r.renewQueueLease(ctx, old, time.Minute); !errors.Is(err, ErrSyncLeaseLost) {
		t.Fatalf("old heartbeat accepted: %v", err)
	}
	newCtx := context.WithValue(ctx, queueLeaseKey{}, current)
	if err = r.storeBusiness(newCtx, "finance", domain.BusinessSnapshot{Metrics: map[string]any{"current": true}}); err != nil {
		t.Fatal(err)
	}
	if err = r.finishQueueJob(ctx, current, nil, false); err != nil {
		t.Fatal(err)
	}
	var count int64
	r.aggDB.Model(&businessRow{}).Count(&count)
	if count != 1 {
		t.Fatal(count)
	}
}

func TestLongRunningSyncJobHeartbeatPreventsPrematureRecovery(t *testing.T) {
	r, job := queueTestRunner(t)
	ctx := context.Background()
	lease := 800 * time.Millisecond
	beat := 100 * time.Millisecond
	l, ok, err := r.claimQueueJob(ctx, job, lease)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	err = r.executeQueueJob(ctx, job, l, lease, beat, 4*time.Second, func(ctx context.Context, _ string, _ string) error {
		deadline := time.NewTimer(1200 * time.Millisecond)
		defer deadline.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
		}
		var observed SyncJob
		if err := r.aggDB.First(&observed, "id=?", "job").Error; err != nil {
			return err
		}
		if _, claimed, err := r.claimQueueJob(ctx, observed, lease); err != nil {
			return err
		} else if claimed {
			return errors.New("healthy long task stolen")
		}
		return r.storeBusiness(ctx, "finance", domain.BusinessSnapshot{Metrics: map[string]any{"heartbeat": true}})
	})
	if err != nil {
		t.Fatal(err)
	}
	var got SyncJob
	r.aggDB.First(&got, "id=?", "job")
	if got.Status != "SUCCESS" || got.LeaseOwner != nil || got.ActiveKey != nil {
		t.Fatalf("completion: %+v", got)
	}
}

func TestSyncJobTimeoutRequeuesWithoutLosingActiveSlot(t *testing.T) {
	r, job := queueTestRunner(t)
	ctx := context.Background()
	l, ok, err := r.claimQueueJob(ctx, job, time.Second)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	err = r.executeQueueJob(ctx, job, l, time.Second, 100*time.Millisecond, 20*time.Millisecond, func(ctx context.Context, _ string, _ string) error { <-ctx.Done(); return ctx.Err() })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout=%v", err)
	}
	var got SyncJob
	r.aggDB.First(&got, "id=?", "job")
	if got.Status != "QUEUED" || got.ActiveKey == nil || got.LeaseOwner != nil {
		t.Fatalf("pause: %+v", got)
	}
}

func TestLegacyRunningRequiresExplicitRecoveryAndCannotResetModernOwner(t *testing.T) {
	r, job := queueTestRunner(t)
	ctx := context.Background()
	if err := r.aggDB.Model(&SyncJob{}).Where("id=?", job.ID).Update("status", "RUNNING").Error; err != nil {
		t.Fatal(err)
	}
	if err := r.RunQueued(ctx); err != nil {
		t.Fatal(err)
	}
	var got SyncJob
	r.aggDB.First(&got, "id=?", job.ID)
	if got.Status != "RUNNING" {
		t.Fatal("live legacy worker was guessed orphaned")
	}
	if err := r.RecoverLegacySyncJobs(ctx, LegacyRecoveryInput{JobIDs: []string{job.ID}, Operator: "test"}); err == nil {
		t.Fatal("unproved recovery accepted")
	}
	if err := r.RecoverLegacySyncJobs(ctx, LegacyRecoveryInput{JobIDs: []string{job.ID}, Operator: "test", VerifyStopped: func(context.Context) ([]byte, error) { return []byte(`{"stopped":true}`), nil }}); err != nil {
		t.Fatal(err)
	}
	r.aggDB.First(&got, "id=?", job.ID)
	if _, ok, err := r.claimQueueJob(ctx, got, time.Minute); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := r.RecoverLegacySyncJobs(ctx, LegacyRecoveryInput{JobIDs: []string{job.ID}, Operator: "test", VerifyStopped: func(context.Context) ([]byte, error) { return []byte(`{"stopped":true}`), nil }}); err == nil {
		t.Fatal("modern owned job reset as legacy")
	}
}

func TestExpiredSyncJobLeaseIsRecoveredAndReleasesActiveKey(t *testing.T) {
	db := newScanTestDB(t)
	if err := db.AutoMigrate(&SyncJob{}); err != nil {
		t.Fatal(err)
	}

	if err := db.Exec(`INSERT INTO sync_job (id,tenant_id,source_id,subsystem_code,status,requested_at,active_key,lease_owner,lease_generation,lease_expires_at) VALUES ('job','tenant','source','settlement','RUNNING',?,'ACTIVE','abandoned',1,?)`, time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	r := &Runner{aggDB: db, tenantID: "tenant", sources: map[string]string{}}
	if err := r.RunQueued(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got SyncJob
	if err := db.First(&got, "id = ?", "job").Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != "FAILED" || got.ActiveKey != nil {
		t.Fatalf("abandoned leased task was not replayed/finalized: status=%s active=%v", got.Status, got.ActiveKey)
	}
}
