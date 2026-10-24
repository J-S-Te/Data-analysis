package aggregation

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
	"gorm.io/gorm"
)

func projectAPI(t *testing.T, r *Runner, h http.HandlerFunc) {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	r.ProjectProjection = ProjectProjectionOptions{URL: s.URL}
	r.ProjectAPI = NewAPISyncRunner(r.aggDB, APISyncOptions{TenantID: r.tenantID, ProjectCredential: MachineCredential{ClientID: "reader"}})
	r.ProjectAPI.tokens["reader"] = cachedMachineToken{value: "synthetic", expiresAt: time.Now().Add(time.Hour)}
}
func TestProjectAPISourceWithoutReadDSN(t *testing.T) {
	r := projectRunner(t)
	r.aggDB.AutoMigrate(&SyncSource{})
	projectAPI(t, r, func(w http.ResponseWriter, q *http.Request) {
		if q.Header.Get("Authorization") != "Bearer synthetic" || q.Header.Get("Accept") != "application/x-ndjson" {
			t.Error("missing machine request")
		}
		w.Write(projectFixture(t))
	})
	if _, ok := r.configuredSources()["project_management"]; !ok {
		t.Fatal("API absent")
	}
	if e := r.EnsureSyncSources(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e := r.runSource(context.Background(), "project_management", ""); e != nil {
		t.Fatal(e)
	}
	if projectCount(t, r, r.tenantID) != 2 {
		t.Fatal("not published")
	}
}
func TestTimerAndQueuedProjectUseSameProducer(t *testing.T) {
	r := projectRunner(t)
	r.aggDB.AutoMigrate(&SyncSource{}, &businessRow{})
	calls := 0
	projectAPI(t, r, func(w http.ResponseWriter, q *http.Request) { calls++; w.Write(projectFixture(t)) })
	if e := r.RunOnce(context.Background()); e != nil {
		t.Fatal(e)
	}
	job := SyncJob{TenantID: r.tenantID, ID: "queued-project", SubsystemCode: "project_management", Status: "QUEUED", RequestedAt: time.Now().UTC()}
	r.aggDB.Create(&job)
	if e := r.RunQueued(context.Background()); e != nil {
		t.Fatal(e)
	}
	r.aggDB.First(&job, "id=?", job.ID)
	if calls != 2 || projectState(t, r).Generation != 2 || job.Status != statusSuccess {
		t.Fatal(calls, job.Status)
	}
	if e := r.syncProject(context.Background(), "invalid-dsn"); e == nil {
		t.Fatal("report failure hidden")
	}
	if projectState(t, r).Status != statusSuccess {
		t.Fatal("projection lost")
	}
}
func TestProjectProjectionLateGenerationCannotPublishOrMarkFailed(t *testing.T) {
	for _, newFails := range []bool{false, true} {
		for _, oldFails := range []bool{false, true} {
			name := fmt.Sprintf("new_failure_%t/old_failure_%t", newFails, oldFails)
			t.Run(name, func(t *testing.T) {
				r := projectRunner(t)
				var calls atomic.Int32
				projectAPI(t, r, func(w http.ResponseWriter, q *http.Request) {
					if calls.Add(1) == 1 {
						if err := r.syncProject(context.Background(), ""); (err != nil) != newFails {
							t.Error(err)
						}
						if oldFails {
							w.WriteHeader(503)
						} else {
							w.Write(projectFixture(t))
						}
						return
					}
					if newFails {
						w.WriteHeader(503)
						return
					}
					w.Write(projectFixture(t))
				})
				if err := r.syncProject(context.Background(), ""); err == nil {
					t.Fatal("old run succeeded")
				}
				state := projectState(t, r)
				want := statusSuccess
				if newFails {
					want = statusFailed
				}
				if state.Generation != 2 || state.Status != want {
					t.Fatal(state)
				}
			})
		}
	}
}
func TestProjectProjectionLicenseExpiresBeforePublish(t *testing.T) {
	r := projectRunner(t)
	var allowed atomic.Bool
	allowed.Store(true)
	r.LicenseCheck = func(context.Context) error {
		if !allowed.Load() {
			return errors.New("expired")
		}
		return nil
	}
	projectAPI(t, r, func(w http.ResponseWriter, q *http.Request) { allowed.Store(false); w.Write(projectFixture(t)) })
	if e := r.syncProject(context.Background(), ""); e == nil {
		t.Fatal("expiry ignored")
	}
	if projectCount(t, r, r.tenantID) != 0 || projectState(t, r).Status != statusRunning {
		t.Fatal("gate bypass")
	}
}
func TestLostQueueLeaseCannotStageOrPublish(t *testing.T) {
	r := projectRunner(t)
	job := SyncJob{TenantID: r.tenantID, ID: "lease", Status: "QUEUED"}
	r.aggDB.Create(&job)
	l, ok, e := r.claimQueueJob(context.Background(), job, time.Minute)
	if e != nil || !ok {
		t.Fatal(e)
	}
	projectAPI(t, r, func(w http.ResponseWriter, q *http.Request) {
		r.aggDB.Model(&SyncJob{}).Where("id=?", job.ID).Update("lease_generation", l.generation+1)
		w.Write(projectFixture(t))
	})
	ctx := context.WithValue(context.Background(), queueLeaseKey{}, l)
	if e := r.syncProject(ctx, ""); !errors.Is(e, ErrSyncLeaseLost) {
		t.Fatal(e)
	}
	var n int64
	r.aggDB.Model(&projectProjectionStage{}).Count(&n)
	if n != 0 || projectCount(t, r, r.tenantID) != 0 || projectState(t, r).Status != statusRunning {
		t.Fatal("lease bypass")
	}
}

func TestProjectHTTPTimeoutAndCancellationRetainLeaseIdentity(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "timeout"}[timeout], func(t *testing.T) {
			r := projectRunner(t)
			job := SyncJob{TenantID: r.tenantID, ID: "cancel-lease", Status: "QUEUED"}
			r.aggDB.Create(&job)
			lease, claimed, claimErr := r.claimQueueJob(context.Background(), job, time.Minute)
			if claimErr != nil || !claimed {
				t.Fatal(claimErr)
			}
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), queueLeaseKey{}, lease))
			defer cancel()
			stopped := make(chan struct{})
			projectAPI(t, r, func(w http.ResponseWriter, q *http.Request) {
				defer close(stopped)
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				if !timeout {
					cancel()
				}
				<-q.Context().Done()
			})
			if timeout {
				r.ProjectProjection.HTTPTimeout = 50 * time.Millisecond
			}
			err := r.syncProject(ctx, "")
			want := context.Canceled
			code := "CANCELED"
			if timeout {
				want = context.DeadlineExceeded
				code = "SOURCE_TIMEOUT"
			}
			if !errors.Is(err, want) {
				t.Fatal(err)
			}
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("response not canceled")
			}
			state := projectState(t, r)
			if state.Status != statusFailed || state.LastError != code || state.LastSuccessAt != nil {
				t.Fatal(state)
			}
		})
	}
}

func TestProjectCommitRechecksLicenseAndRollsBack(t *testing.T) {
	r := projectRunner(t)
	importProject(t, r, projectFixture(t))
	original := projectState(t, r)
	allowed := true
	r.LicenseCheck = func(context.Context) error {
		if !allowed {
			return errors.New("expired at commit")
		}
		return nil
	}
	if err := r.aggDB.Callback().Raw().After("gorm:raw").Register("test:expire-project-license", func(tx *gorm.DB) {
		if strings.HasPrefix(tx.Statement.SQL.String(), "INSERT INTO dim_project") {
			allowed = false
		}
	}); err != nil {
		t.Fatal(err)
	}
	projectAPI(t, r, func(w http.ResponseWriter, q *http.Request) { w.Write(projectFixture(t)) })
	if err := r.syncProject(context.Background(), ""); err == nil {
		t.Fatal("commit ignored license")
	}
	state := projectState(t, r)
	if state.Status != statusRunning || state.PublishedRunID != original.PublishedRunID || projectCount(t, r, r.tenantID) != 2 {
		t.Fatal("commit did not roll back", state)
	}
}

func TestOldTimerCannotOverwriteNewSourceStatus(t *testing.T) {
	r := projectRunner(t)
	r.aggDB.AutoMigrate(&SyncSource{})
	r.aggDB.Create(&SyncSource{TenantID: r.tenantID, ID: "project", SubsystemCode: "project_management"})
	now := time.Now().UTC()
	old := now.Add(-time.Minute)
	r.mark(context.Background(), "project_management", statusRunning, old, nil)
	newError := "new run failure"
	r.mark(context.Background(), "project_management", statusFailed, now, &newError)
	r.mark(context.Background(), "project_management", statusSuccess, old, nil)
	var source SyncSource
	r.aggDB.First(&source, "id=?", "project")
	if source.LastStatus != statusFailed || source.LastError == nil || *source.LastError != newError {
		t.Fatal("stale status overwrote newer run", source)
	}
}

func TestProjectProjectionOptionsRejectUnsafeExplicitLimits(t *testing.T) {
	for _, options := range []ProjectProjectionOptions{{HTTPTimeout: -time.Second}, {HTTPTimeout: 36 * time.Second}, {MaxProjects: -1}, {MaxProjects: 50001}, {MaxBytes: 67108865}, {MaxLineBytes: 65537}, {URL: "file:///tmp/snapshot"}, {URL: "http://user:password@example.test"}} {
		if ValidateProjectProjectionOptions(options) == nil {
			t.Fatal("invalid options accepted", options)
		}
	}
}

func TestProjectProjectionFailurePreservesReportSuccess(t *testing.T) {
	r := projectRunner(t)
	if err := r.aggDB.AutoMigrate(&businessRow{}); err != nil {
		t.Fatal(err)
	}
	if err := r.storeBusiness(context.Background(), "report", domain.BusinessSnapshot{Metrics: map[string]any{"active_reports": 7}}); err != nil {
		t.Fatal(err)
	}
	var before businessRow
	r.aggDB.First(&before, "tenant_id=? AND section=?", r.tenantID, "report")
	projectAPI(t, r, func(w http.ResponseWriter, q *http.Request) { w.Write([]byte("incomplete\n")) })
	if err := r.syncProject(context.Background(), ""); err == nil {
		t.Fatal("invalid projection accepted")
	}
	var after businessRow
	r.aggDB.First(&after, "tenant_id=? AND section=?", r.tenantID, "report")
	if after.Payload != before.Payload || !after.SnapshotAt.Equal(before.SnapshotAt) {
		t.Fatal("report success erased")
	}
}

func TestLostQueueLeaseCannotPublishStagedProjection(t *testing.T) {
	r := projectRunner(t)
	job := SyncJob{TenantID: r.tenantID, ID: "publish-lease", Status: "QUEUED"}
	r.aggDB.Create(&job)
	l, ok, err := r.claimQueueJob(context.Background(), job, time.Minute)
	if err != nil || !ok {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), queueLeaseKey{}, l)
	a, err := r.beginProjectProjection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := readProjectSnapshot(ctx, bytes.NewReader(projectFixture(t)), r.tenantID, ProjectProjectionOptions{}, projectProjectionSink{r, a})
	if err != nil {
		t.Fatal(err)
	}
	r.aggDB.Model(&SyncJob{}).Where("id=?", job.ID).Update("lease_generation", l.generation+1)
	if err = r.publishProjectProjection(ctx, a, summary); !errors.Is(err, ErrSyncLeaseLost) {
		t.Fatal(err)
	}
	if err = r.failProjectProjection(ctx, a, "PUBLISH_FAILED"); !errors.Is(err, ErrSyncLeaseLost) {
		t.Fatal(err)
	}
	if projectCount(t, r, r.tenantID) != 0 || projectState(t, r).Status != statusRunning {
		t.Fatal("lost owner published or marked failed")
	}
}

func TestMachineTokenRefreshWaitRespectsCancellation(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "deadline"}[deadline], func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
				requests.Add(1)
				close(started)
				<-release
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer server.Close()
			runner := NewAPISyncRunner(nil, APISyncOptions{MachineTokenURL: server.URL, HTTPTimeout: time.Second})
			credential := MachineCredential{ClientID: "synthetic-reader", ClientSecret: "synthetic-secret", Scope: "dashboard.project.read"}
			owner := make(chan error, 1)
			go func() { _, err := runner.machineToken(context.Background(), credential); owner <- err }()
			select {
			case <-started:
			case <-time.After(time.Second):
				close(release)
				t.Fatal("refresh did not start")
			}
			ctx, cancel := context.WithCancel(context.Background())
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
			}
			defer cancel()
			waiter := make(chan error, 1)
			go func() { _, err := runner.machineToken(ctx, credential); waiter <- err }()
			if !deadline {
				cancel()
			}
			var err error
			var blocked bool
			select {
			case err = <-waiter:
			case <-time.After(150 * time.Millisecond):
				blocked = true
			}
			// Release the owner even on RED, so failure cannot leak a test goroutine.
			close(release)
			if ownerErr := <-owner; ownerErr == nil {
				t.Error("503 refresh accepted")
			}
			if blocked {
				<-waiter
				t.Fatal("token waiter still blocked after cancellation/deadline")
			}
			want := context.Canceled
			if deadline {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) {
				t.Fatal(err)
			}
			if requests.Load() != 1 {
				t.Fatal("canceled waiter issued a request", requests.Load())
			}
		})
	}
}

func TestMachineTokenConcurrentRefreshSharesVerifiedCache(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	claims := `{"sub":"synthetic-reader","token_use":"application","scope":["dashboard.project.read"]}`
	token := "e30." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".synthetic-signature"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if requests.Add(1) == 1 {
			close(started)
		}
		<-release
		fmt.Fprintf(w, `{"access_token":%q,"expires_in":3600}`, token)
	}))
	defer server.Close()
	runner := NewAPISyncRunner(nil, APISyncOptions{MachineTokenURL: server.URL, HTTPTimeout: time.Second})
	credential := MachineCredential{ClientID: "synthetic-reader", ClientSecret: "synthetic-secret", Scope: "dashboard.project.read"}
	results := make(chan error, 16)
	for i := 0; i < 16; i++ {
		go func() {
			value, err := runner.machineToken(context.Background(), credential)
			if err == nil && value != token {
				err = errors.New("wrong cached token")
			}
			results <- err
		}()
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("refresh did not start")
	}
	close(release)
	for i := 0; i < 16; i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("cache waiter stranded")
		}
	}
	if requests.Load() != 1 {
		t.Fatal("cache refresh duplicated", requests.Load())
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if _, err := runner.machineToken(canceled, credential); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled caller read cached token", err)
	}
}
