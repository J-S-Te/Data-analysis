package aggregation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// These tests catch partial publication, permissive decoding and stale writers.
// Frozen B1 synthetic fixture, copied byte-for-byte so the BI repository remains standalone.
// Source: .ai/qa/project-temporal-bi-20261023/sdd/project-snapshot-v1.ndjson.
const projectFixtureV1 = `{"type":"header","schema_version":1,"tenant_id":"synthetic-tenant","snapshot_id":"synthetic-project-snapshot-v1","source_snapshot_at":"2026-10-23T00:00:00.000000123Z"}
{"type":"project","tenant_id":"synthetic-tenant","snapshot_id":"synthetic-project-snapshot-v1","id":"P-001","name":"合成项目：交付完成且部分终止","customer":"合成客户","customer_id":"synthetic-customer","contract":"SYNTHETIC-HT-001","contract_id":"synthetic-contract","category":"合成检测类别","team":"合成团队展示值","manager":"合成经理","manager_identity_id":"synthetic-manager","owner_org_id":"synthetic-org","owner_identity_id":"synthetic-owner","status":"已完成","progress":100,"risk":true,"in_flight":false,"service_items":2,"created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-20T00:00:00Z","source_updated_at":"2026-10-22T00:00:00Z"}
{"type":"project","tenant_id":"synthetic-tenant","snapshot_id":"synthetic-project-snapshot-v1","id":"P-002","name":"合成项目：等待实施","customer":"合成客户","customer_id":"synthetic-customer","contract":"SYNTHETIC-HT-001","contract_id":"synthetic-contract","category":"合成检测类别","team":"合成团队展示值","manager":"合成经理","manager_identity_id":"synthetic-manager","owner_org_id":"synthetic-org","owner_identity_id":"synthetic-owner","status":"待实施","progress":0,"risk":false,"in_flight":true,"service_items":0,"created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-20T00:00:00Z","source_updated_at":"2026-10-22T00:00:00Z"}
{"type":"end","schema_version":1,"tenant_id":"synthetic-tenant","snapshot_id":"synthetic-project-snapshot-v1","total":2,"checksum_sha256":"af567d6d4df4bd3f64b476bbf8c692a1e75bed08d53df2af26147657c38ea561"}
`

func projectFixture(t *testing.T) []byte {
	t.Helper()
	return []byte(projectFixtureV1)
}
func projectWire(t *testing.T, rows ...map[string]any) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(projectFixture(t)), []byte{'\n'})
	var h, e map[string]any
	json.Unmarshal(lines[0], &h)
	json.Unmarshal(lines[3], &e)
	var b bytes.Buffer
	first, _ := json.Marshal(h)
	b.Write(first)
	b.WriteByte('\n')
	hash := sha256.New()
	for _, r := range rows {
		line, _ := json.Marshal(r)
		line = append(line, '\n')
		b.Write(line)
		hash.Write(line)
	}
	e["total"] = len(rows)
	e["checksum_sha256"] = hex.EncodeToString(hash.Sum(nil))
	last, _ := json.Marshal(e)
	b.Write(last)
	b.WriteByte('\n')
	return b.Bytes()
}
func projectMap(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	json.Unmarshal(bytes.Split(projectFixture(t), []byte{'\n'})[1], &m)
	return m
}

type collectingProjectSink struct {
	rows    []ProjectProjection
	batches []int
	err     error
}

func (s *collectingProjectSink) Stage(_ context.Context, rows []ProjectProjection) error {
	s.rows = append(s.rows, rows...)
	s.batches = append(s.batches, len(rows))
	return s.err
}
func TestProjectStreamRejectsIncompleteOrConflictingSnapshot(t *testing.T) {
	good := projectFixture(t)
	lines := bytes.Split(bytes.TrimSpace(good), []byte{'\n'})
	cases := map[string][]byte{"empty": nil, "null": []byte("null\n"), "missing_header": bytes.Join(lines[1:], []byte{'\n'}), "missing_end": append(bytes.Join(lines[:3], []byte{'\n'}), '\n'), "after_end": append(append([]byte{}, good...), []byte("{}\n")...), "bad_hash": bytes.Replace(good, []byte("af567d"), []byte("000000"), 1), "bad_total": bytes.Replace(good, []byte(`"total":2`), []byte(`"total":3`), 1), "duplicate_key": bytes.Replace(good, []byte(`"progress":100`), []byte(`"progress":100,"progress":100`), 1), "invalid_utf8": bytes.Replace(good, []byte("合成客户"), []byte{0xff}, 1)}
	for name, change := range map[string]func(map[string]any){"tenant": func(m map[string]any) { m["tenant_id"] = "other" }, "snapshot": func(m map[string]any) { m["snapshot_id"] = "other" }, "missing_field": func(m map[string]any) { delete(m, "risk") }, "null_field": func(m map[string]any) { m["risk"] = nil }, "unknown_field": func(m map[string]any) { m["extra"] = true }, "progress": func(m map[string]any) { m["progress"] = 101 }, "negative_items": func(m map[string]any) { m["service_items"] = -1 }, "service_limit": func(m map[string]any) { m["service_items"] = 250001 }, "invalid_time": func(m map[string]any) { m["created_at"] = "2026-02-30T00:00:00Z" }, "zero_time": func(m map[string]any) { m["created_at"] = "0001-01-01T00:00:00Z" }, "offset": func(m map[string]any) { m["created_at"] = "2026-10-01T00:00:00+00:00" }, "time_order": func(m map[string]any) { m["updated_at"] = "2026-09-01T00:00:00Z" }, "source_time": func(m map[string]any) { m["source_updated_at"] = "2026-10-01T00:00:00Z" }} {
		m := projectMap(t)
		change(m)
		cases[name] = projectWire(t, m)
	}
	cases["partial_after_end"] = append(append([]byte{}, good...), []byte("{}")...)
	cases["whitespace_after_end"] = append(append([]byte{}, good...), ' ')
	cases["no_end_newline"] = bytes.TrimSuffix(good, []byte{'\n'})
	cases["raw_hash_not_reencoded"] = bytes.Replace(good, []byte(`"progress":100`), []byte(`"progress": 100`), 1)
	cases["header_version"] = bytes.Replace(good, []byte(`"schema_version":1`), []byte(`"schema_version":2`), 1)
	cases["header_tenant"] = bytes.Replace(good, []byte(`"tenant_id":"synthetic-tenant"`), []byte(`"tenant_id":"other"`), 1)
	m := projectMap(t)
	cases["duplicate_row"] = projectWire(t, m, m)
	n := projectMap(t)
	n["id"] = "P-000"
	cases["order"] = projectWire(t, m, n)
	for name, wire := range cases {
		t.Run(name, func(t *testing.T) {
			if _, e := readProjectSnapshot(context.Background(), bytes.NewReader(wire), "synthetic-tenant", ProjectProjectionOptions{}, &collectingProjectSink{}); e == nil {
				t.Fatal("invalid snapshot accepted")
			}
		})
	}
	for name, opt := range map[string]ProjectProjectionOptions{"line": {MaxLineBytes: 200}, "bytes": {MaxBytes: 1000}, "projects": {MaxProjects: 1}, "bad_options": {MaxProjects: 50001}, "timeout": {HTTPTimeout: -time.Second}} {
		t.Run(name, func(t *testing.T) {
			if _, e := readProjectSnapshot(context.Background(), bytes.NewReader(good), "synthetic-tenant", opt, &collectingProjectSink{}); e == nil {
				t.Fatal("budget accepted")
			}
		})
	}
	for _, wire := range [][]byte{good, projectWire(t)} {
		sink := &collectingProjectSink{}
		s, e := readProjectSnapshot(context.Background(), bytes.NewReader(wire), "synthetic-tenant", ProjectProjectionOptions{}, sink)
		if e != nil {
			t.Fatal(e)
		}
		if s.Total != len(sink.rows) {
			t.Fatal("summary total")
		}
		if s.Total == 2 && (sink.rows[0].InFlight || !sink.rows[0].Risk || sink.rows[1].Progress != 0) {
			t.Fatal("source booleans changed")
		}
	}
	// A source timestamp after read start is valid: first consistent read can be later.
	m = projectMap(t)
	m["source_updated_at"] = "2026-10-24T00:00:00Z"
	if _, e := readProjectSnapshot(context.Background(), bytes.NewReader(projectWire(t, m)), "synthetic-tenant", ProjectProjectionOptions{}, &collectingProjectSink{}); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := readProjectSnapshot(ctx, bytes.NewReader(good), "synthetic-tenant", ProjectProjectionOptions{}, &collectingProjectSink{}); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if _, e := readProjectSnapshot(context.Background(), bytes.NewReader(good), "synthetic-tenant", ProjectProjectionOptions{}, &collectingProjectSink{err: errors.New("sink failed")}); e == nil {
		t.Fatal("sink failure hidden")
	}
}

type projectAttemptSink struct {
	r *Runner
	a ProjectionAttempt
}

func (s projectAttemptSink) Stage(ctx context.Context, rows []ProjectProjection) error {
	return s.r.stageProjectProjection(ctx, s.a, rows)
}
func projectRunner(t *testing.T) *Runner {
	t.Helper()
	db := newScanTestDB(t)
	if e := db.AutoMigrate(&ProjectProjection{}, &projectProjectionStage{}, &ProjectProjectionState{}, &SyncJob{}); e != nil {
		t.Fatal(e)
	}
	return &Runner{aggDB: db, tenantID: "synthetic-tenant"}
}
func projectCount(t *testing.T, r *Runner, tenant string) int64 {
	t.Helper()
	var n int64
	if e := r.aggDB.Model(&ProjectProjection{}).Where("tenant_id=?", tenant).Count(&n).Error; e != nil {
		t.Fatal(e)
	}
	return n
}
func importProject(t *testing.T, r *Runner, wire []byte) ProjectionAttempt {
	t.Helper()
	ctx := context.Background()
	a, e := r.beginProjectProjection(ctx)
	if e != nil {
		t.Fatal(e)
	}
	s, e := readProjectSnapshot(ctx, bytes.NewReader(wire), r.tenantID, ProjectProjectionOptions{}, projectAttemptSink{r, a})
	if e != nil {
		t.Fatal(e)
	}
	if e = r.publishProjectProjection(ctx, a, s); e != nil {
		t.Fatal(e)
	}
	return a
}
func projectState(t *testing.T, r *Runner) ProjectProjectionState {
	t.Helper()
	var s ProjectProjectionState
	if e := r.aggDB.First(&s, "tenant_id=?", r.tenantID).Error; e != nil {
		t.Fatal(e)
	}
	return s
}
func TestProjectPublicationIsAtomicAndTenantScoped(t *testing.T) {
	r := projectRunner(t)
	ctx := context.Background()
	// Legacy rows lack a trusted success marker until a full stream is published.
	legacy := ProjectProjection{TenantID: r.tenantID, ID: "legacy"}
	if e := r.aggDB.Create(&legacy).Error; e != nil {
		t.Fatal(e)
	}
	a, e := r.beginProjectProjection(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if projectState(t, r).LastSuccessAt != nil {
		t.Fatal("legacy falsely successful")
	}
	s, e := readProjectSnapshot(ctx, bytes.NewReader(projectFixture(t)), r.tenantID, ProjectProjectionOptions{}, projectAttemptSink{r, a})
	if e != nil {
		t.Fatal(e)
	}
	if projectCount(t, r, r.tenantID) != 1 {
		t.Fatal("staging changed published rows")
	}
	if e = r.failProjectProjection(ctx, a, "raw secret token"); e != nil {
		t.Fatal(e)
	}
	if projectCount(t, r, r.tenantID) != 1 || strings.Contains(projectState(t, r).LastError, "secret") {
		t.Fatal("failure changed data/leaked error")
	}
	if e = r.publishProjectProjection(ctx, a, s); e == nil {
		t.Fatal("failed attempt published")
	}
	importProject(t, r, projectFixture(t))
	before := projectState(t, r)
	if before.LastSuccessAt == nil || before.PublishedTotal != 2 {
		t.Fatal(before)
	}
	other := ProjectProjection{TenantID: "other", ID: "other"}
	if e = r.aggDB.Create(&other).Error; e != nil {
		t.Fatal(e)
	}
	m := projectMap(t)
	m["owner_org_id"] = "new-org"
	importProject(t, r, projectWire(t, m))
	var p ProjectProjection
	r.aggDB.First(&p, "tenant_id=?", r.tenantID)
	if p.OwnerOrgID != "new-org" || projectCount(t, r, r.tenantID) != 1 {
		t.Fatal("org/deletion not replaced")
	}
	a, e = r.beginProjectProjection(ctx)
	if e != nil {
		t.Fatal(e)
	}
	old := projectState(t, r)
	if e = r.failProjectProjection(ctx, a, "INVALID_STREAM"); e != nil {
		t.Fatal(e)
	}
	after := projectState(t, r)
	if !after.LastSuccessAt.Equal(*old.LastSuccessAt) || after.PublishedTotal != 1 {
		t.Fatal("lost last success")
	}
	importProject(t, r, projectWire(t))
	if projectCount(t, r, r.tenantID) != 0 || projectCount(t, r, "other") != 1 || projectState(t, r).PublishedTotal != 0 {
		t.Fatal("empty snapshot scope")
	}
}
func TestProjectGenerationAndPublicationRollback(t *testing.T) {
	r := projectRunner(t)
	ctx := context.Background()
	importProject(t, r, projectFixture(t))
	prior := projectState(t, r)
	old, e := r.beginProjectProjection(ctx)
	if e != nil {
		t.Fatal(e)
	}
	s, e := readProjectSnapshot(ctx, bytes.NewReader(projectFixture(t)), r.tenantID, ProjectProjectionOptions{}, projectAttemptSink{r, old})
	if e != nil {
		t.Fatal(e)
	}
	current, e := r.beginProjectProjection(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if current.Generation <= old.Generation {
		t.Fatal("generation not advanced")
	}
	for name, run := range map[string]func() error{"stage": func() error { return r.stageProjectProjection(ctx, old, []ProjectProjection{}) }, "publish": func() error { return r.publishProjectProjection(ctx, old, s) }, "fail": func() error { return r.failProjectProjection(ctx, old, "INVALID_STREAM") }} {
		if e := run(); !errors.Is(e, ErrProjectProjectionSuperseded) {
			t.Fatalf("%s: %v", name, e)
		}
	}
	if projectState(t, r).Status != "RUNNING" {
		t.Fatal("stale failure overwrote new run")
	}
	s, e = readProjectSnapshot(ctx, bytes.NewReader(projectFixture(t)), r.tenantID, ProjectProjectionOptions{}, projectAttemptSink{r, current})
	if e != nil {
		t.Fatal(e)
	}
	// Force an actual SQLite insert failure after DELETE. Both must roll back.
	if e = r.aggDB.Exec("CREATE TRIGGER reject_project BEFORE INSERT ON dim_project BEGIN SELECT RAISE(ABORT, 'write failed'); END").Error; e != nil {
		t.Fatal(e)
	}
	if e = r.publishProjectProjection(ctx, current, s); e == nil {
		t.Fatal("insert failure ignored")
	}
	if got := projectState(t, r); !got.LastSuccessAt.Equal(*prior.LastSuccessAt) || got.PublishedRunID != prior.PublishedRunID || projectCount(t, r, r.tenantID) != 2 {
		t.Fatal("rows or watermark changed despite failed insert")
	}
	r.aggDB.Exec("DROP TRIGGER reject_project")
	if e = r.publishProjectProjection(ctx, current, s); e != nil {
		t.Fatal(e)
	}
	var count int64
	r.aggDB.Model(&projectProjectionStage{}).Where("run_id=?", old.RunID).Count(&count)
	if count != 0 {
		t.Fatal("superseded staging was not reclaimed")
	}
}
func TestProjectWritesRespectLicenseAndQueueLease(t *testing.T) {
	r := projectRunner(t)
	ctx := context.Background()
	a, e := r.beginProjectProjection(ctx)
	if e != nil {
		t.Fatal(e)
	}
	denied := errors.New("license denied")
	r.LicenseCheck = func(context.Context) error { return denied }
	if _, e = r.beginProjectProjection(ctx); !errors.Is(e, denied) {
		t.Fatal(e)
	}
	if e = r.failProjectProjection(ctx, a, "INVALID_STREAM"); !errors.Is(e, denied) {
		t.Fatal(e)
	}
	r.LicenseCheck = nil
	leaseCtx := context.WithValue(ctx, queueLeaseKey{}, queueLease{tenant: r.tenantID, job: "missing", owner: "old", generation: 1})
	if e = r.stageProjectProjection(leaseCtx, a, nil); !errors.Is(e, ErrSyncLeaseLost) {
		t.Fatal(e)
	}
	if projectState(t, r).Status != "RUNNING" {
		t.Fatal("gate changed state")
	}
	// A malformed direct staging call cannot bypass the reader's contract.
	if e = r.stageProjectProjection(ctx, a, []ProjectProjection{{TenantID: "other", ID: "P"}}); e == nil {
		t.Fatal("cross tenant stage accepted")
	}
}

func TestProjectStreamBatchingBudgetsAndFixturePrecision(t *testing.T) {
	rows := make([]map[string]any, 401)
	for i := range rows {
		rows[i] = projectMap(t)
		rows[i]["id"] = fmt.Sprintf("P-%04d", i)
	}
	wire := projectWire(t, rows...)
	sink := &collectingProjectSink{}
	summary, err := readProjectSnapshot(context.Background(), bytes.NewReader(wire), "synthetic-tenant", ProjectProjectionOptions{MaxBytes: len(wire)}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(sink.batches) != "[200 200 1]" || summary.Total != 401 || summary.SourceSnapshotAt.Nanosecond() != 123 {
		t.Fatalf("batches/precision: %v %+v", sink.batches, summary)
	}
	if _, err = readProjectSnapshot(context.Background(), bytes.NewReader(wire), "synthetic-tenant", ProjectProjectionOptions{MaxBytes: len(wire) - 1}, &collectingProjectSink{}); err == nil {
		t.Fatal("end bytes escaped total budget")
	}
	r := projectRunner(t)
	importProject(t, r, projectFixture(t))
	a, err := r.beginProjectProjection(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cutoff := bytes.LastIndex(wire, []byte(`{"checksum_sha256"`))
	if cutoff < 0 {
		t.Fatal("end fixture")
	}
	failed, err := readProjectSnapshot(context.Background(), bytes.NewReader(wire[:cutoff]), r.tenantID, ProjectProjectionOptions{}, projectAttemptSink{r, a})
	if err == nil || failed.verified {
		t.Fatal("truncated stream accepted")
	}
	if projectCount(t, r, r.tenantID) != 2 {
		t.Fatal("partial stream published")
	}
	if err = r.publishProjectProjection(context.Background(), a, failed); err == nil {
		t.Fatal("invalid summary published")
	}
	if err = r.failProjectProjection(context.Background(), a, "INVALID_STREAM"); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err = r.aggDB.Model(&projectProjectionStage{}).Where("tenant_id=? AND run_id=?", r.tenantID, a.RunID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("failed run cleanup: %d %v", count, err)
	}
	first, second := projectMap(t), projectMap(t)
	first["service_items"] = 125001
	second["id"] = "P-002"
	second["service_items"] = 125000
	if _, err = readProjectSnapshot(context.Background(), bytes.NewReader(projectWire(t, first, second)), r.tenantID, ProjectProjectionOptions{}, &collectingProjectSink{}); err == nil {
		t.Fatal("cumulative service item limit ignored")
	}
	empty, err := readProjectSnapshot(context.Background(), bytes.NewReader(projectWire(t)), r.tenantID, ProjectProjectionOptions{}, &collectingProjectSink{})
	if err != nil || empty.SourceUpdatedAt != nil || empty.ChecksumSHA256 != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("empty summary: %+v %v", empty, err)
	}
}
func TestProjectDirectStagingEnforcesUTCAndBatchIsolation(t *testing.T) {
	r := projectRunner(t)
	ctx := context.Background()
	a, err := r.beginProjectProjection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sink := &collectingProjectSink{}
	_, err = readProjectSnapshot(ctx, bytes.NewReader(projectFixture(t)), r.tenantID, ProjectProjectionOptions{}, sink)
	if err != nil {
		t.Fatal(err)
	}
	row := sink.rows[0]
	row.CreatedAt = row.CreatedAt.In(time.FixedZone("offset", 3600))
	if err = r.stageProjectProjection(ctx, a, []ProjectProjection{row}); err == nil {
		t.Fatal("direct staging accepted non-UTC timestamp")
	}
	row = sink.rows[0]
	if err = r.stageProjectProjection(ctx, a, []ProjectProjection{row}); err != nil {
		t.Fatal(err)
	}
	row.ID = "P-003"
	row.SnapshotID = "conflicting"
	if err = r.stageProjectProjection(ctx, a, []ProjectProjection{row}); err == nil {
		t.Fatal("mixed snapshot stage accepted")
	}
	var n int64
	if err = r.aggDB.Model(&projectProjectionStage{}).Where("run_id=?", a.RunID).Count(&n).Error; err != nil || n != 1 {
		t.Fatalf("conflicting batch failed to rollback: %d %v", n, err)
	}
	if err = r.stageProjectProjection(ctx, a, make([]ProjectProjection, 201)); err == nil {
		t.Fatal("oversized stage batch accepted")
	}
}

func TestProjectEveryWriteRechecksLicenseAndLease(t *testing.T) {
	r := projectRunner(t)
	ctx := context.Background()
	a, err := r.beginProjectProjection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sink := &collectingProjectSink{}
	summary, err := readProjectSnapshot(ctx, bytes.NewReader(projectFixture(t)), r.tenantID, ProjectProjectionOptions{}, sink)
	if err != nil {
		t.Fatal(err)
	}
	denied := errors.New("denied")
	r.LicenseCheck = func(context.Context) error { return denied }
	for name, write := range map[string]func() error{
		"stage":   func() error { return r.stageProjectProjection(ctx, a, sink.rows) },
		"publish": func() error { return r.publishProjectProjection(ctx, a, summary) },
	} {
		if err = write(); !errors.Is(err, denied) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	r.LicenseCheck = nil
	owner := "expired"
	expired := time.Now().UTC().Add(-time.Minute)
	job := SyncJob{ID: "expired-job", TenantID: r.tenantID, Status: "RUNNING", LeaseOwner: &owner, LeaseGeneration: 1, LeaseExpiresAt: &expired}
	if err = r.aggDB.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	leaseCtx := context.WithValue(ctx, queueLeaseKey{}, queueLease{tenant: r.tenantID, job: job.ID, owner: owner, generation: 1})
	for name, write := range map[string]func() error{
		"begin":   func() error { _, e := r.beginProjectProjection(leaseCtx); return e },
		"stage":   func() error { return r.stageProjectProjection(leaseCtx, a, sink.rows) },
		"publish": func() error { return r.publishProjectProjection(leaseCtx, a, summary) },
		"fail":    func() error { return r.failProjectProjection(leaseCtx, a, "INVALID_STREAM") },
	} {
		if err = write(); !errors.Is(err, ErrSyncLeaseLost) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if projectState(t, r).Status != "RUNNING" || projectCount(t, r, r.tenantID) != 0 {
		t.Fatal("write bypassed gate")
	}
}

type projectSnapshotSinkFunc func(context.Context, []ProjectProjection) error

func (f projectSnapshotSinkFunc) Stage(ctx context.Context, rows []ProjectProjection) error {
	return f(ctx, rows)
}

func TestProjectFinalStageCancellationRejectsSummary(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			options := ProjectProjectionOptions{}
			want := context.Canceled
			if mode == "deadline" {
				options.HTTPTimeout = 50 * time.Millisecond
				want = context.DeadlineExceeded
			}
			calls := 0
			sink := projectSnapshotSinkFunc(func(stageCtx context.Context, rows []ProjectProjection) error {
				calls++
				if len(rows) != 2 {
					t.Fatalf("final batch rows = %d, want 2", len(rows))
				}
				if mode == "cancel" {
					cancel()
				} else {
					select {
					case <-stageCtx.Done():
					case <-time.After(time.Second):
						t.Fatal("reader deadline did not reach final Stage")
					}
				}
				return nil
			})
			summary, err := readProjectSnapshot(ctx, bytes.NewReader(projectFixture(t)), "synthetic-tenant", options, sink)
			if calls != 1 {
				t.Fatalf("final Stage calls = %d, want 1", calls)
			}
			if !errors.Is(err, want) || summary != (ProjectSnapshotSummary{}) {
				t.Fatalf("err = %v, summary = %+v; want %v and empty summary", err, summary, want)
			}
		})
	}
}

func TestProjectOrphanStagingReclaimedAfterSupersession(t *testing.T) {
	assertProjectOrphanReclamation(t, projectRunner(t))
}
func assertProjectOrphanReclamation(t *testing.T, r *Runner) {
	t.Helper()
	ctx := context.Background()
	old, e := r.beginProjectProjection(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = readProjectSnapshot(ctx, bytes.NewReader(projectFixture(t)), r.tenantID, ProjectProjectionOptions{}, projectAttemptSink{r, old}); e != nil {
		t.Fatal(e)
	}
	foreign := projectProjectionStage{ProjectProjection: ProjectProjection{TenantID: "other", ID: "foreign", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), SourceUpdatedAt: time.Now().UTC()}, RunID: "foreign"}
	if e = r.aggDB.Create(&foreign).Error; e != nil {
		t.Fatal(e)
	}
	current, e := r.beginProjectProjection(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = r.failProjectProjection(ctx, old, "CANCELED"); !errors.Is(e, ErrProjectProjectionSuperseded) {
		t.Fatal(e)
	}
	var n int64
	r.aggDB.Model(&projectProjectionStage{}).Where("tenant_id=? AND run_id=?", r.tenantID, old.RunID).Count(&n)
	if n != 0 {
		t.Fatalf("orphan staging retained: %d", n)
	}
	r.aggDB.Model(&projectProjectionStage{}).Where("tenant_id=?", "other").Count(&n)
	if n != 1 {
		t.Fatal("foreign staging deleted")
	}
	if _, e = readProjectSnapshot(ctx, bytes.NewReader(projectFixture(t)), r.tenantID, ProjectProjectionOptions{}, projectAttemptSink{r, current}); e != nil {
		t.Fatal(e)
	}
	r.aggDB.Model(&projectProjectionStage{}).Where("tenant_id=? AND run_id=?", r.tenantID, current.RunID).Count(&n)
	if n != 2 {
		t.Fatal("current staging lost")
	}
}

func TestProjectStagingReclamationLicenseRenewalAndBatching(t *testing.T) {
	r := projectRunner(t)
	ctx := context.Background()
	old, e := r.beginProjectProjection(ctx)
	if e != nil {
		t.Fatal(e)
	}
	// A crashed process can leave a full batch plus a tail.
	var rows []projectProjectionStage
	for i := 0; i < 201; i++ {
		rows = append(rows, projectProjectionStage{ProjectProjection: ProjectProjection{TenantID: r.tenantID, ID: fmt.Sprintf("p-%03d", i)}, RunID: old.RunID})
	}
	if e = r.aggDB.Create(&rows).Error; e != nil {
		t.Fatal(e)
	}
	denied := errors.New("license denied")
	r.LicenseCheck = func(context.Context) error { return denied }
	if _, e = r.beginProjectProjection(ctx); !errors.Is(e, denied) {
		t.Fatal(e)
	}
	var n int64
	r.aggDB.Model(&projectProjectionStage{}).Where("tenant_id=?", r.tenantID).Count(&n)
	if n != 201 {
		t.Fatal("unauthorized GC")
	}
	r.LicenseCheck = func(context.Context) error { return nil }
	if _, e = r.beginProjectProjection(context.WithValue(ctx, queueLeaseKey{}, queueLease{tenant: r.tenantID, job: "missing", owner: "expired", generation: 1})); !errors.Is(e, ErrSyncLeaseLost) {
		t.Fatal(e)
	}
	r.aggDB.Model(&projectProjectionStage{}).Where("tenant_id=?", r.tenantID).Count(&n)
	if n != 201 {
		t.Fatal("expired lease GC")
	}
	if _, e = r.beginProjectProjection(ctx); e != nil {
		t.Fatal(e)
	}
	r.aggDB.Model(&projectProjectionStage{}).Where("tenant_id=?", r.tenantID).Count(&n)
	if n != 0 {
		t.Fatal("renewal did not reclaim backlog")
	}
}
