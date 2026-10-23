package aggregation

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const maxProjectServiceItems = 250000

func (o ProjectProjectionOptions) normalized() (ProjectProjectionOptions, error) {
	if o.HTTPTimeout == 0 {
		o.HTTPTimeout = 35 * time.Second
	}
	if o.MaxProjects == 0 {
		o.MaxProjects = 50000
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = 64 << 20
	}
	if o.MaxLineBytes == 0 {
		o.MaxLineBytes = 64 << 10
	}
	if o.HTTPTimeout <= 0 || o.HTTPTimeout > 35*time.Second || o.MaxProjects < 1 || o.MaxProjects > 50000 || o.MaxBytes < 1 || o.MaxBytes > 64<<20 || o.MaxLineBytes < 1 || o.MaxLineBytes > 64<<10 {
		return o, errors.New("invalid project projection limits")
	}
	return o, nil
}

var projectHeaderFields = strings.Fields("type schema_version tenant_id snapshot_id source_snapshot_at")
var projectRowFields = strings.Fields("type tenant_id snapshot_id id name customer customer_id contract contract_id category team manager manager_identity_id owner_org_id owner_identity_id status progress risk in_flight service_items created_at updated_at source_updated_at")
var projectEndFields = strings.Fields("type schema_version tenant_id snapshot_id total checksum_sha256")

// Decode an exact object, rejecting duplicate, missing, null and unknown fields.
// encoding/json alone accepts duplicate keys and replaces invalid UTF-8.
func projectObject(raw []byte, fields []string, out any) (map[string]json.RawMessage, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("invalid project snapshot UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, e := d.Token()
	if e != nil || token != json.Delim('{') {
		return nil, errors.New("expected project snapshot object")
	}
	values := make(map[string]json.RawMessage, len(fields))
	for d.More() {
		token, e = d.Token()
		if e != nil {
			return nil, e
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("invalid field")
		}
		if _, ok = values[key]; ok {
			return nil, errors.New("duplicate project snapshot field")
		}
		var value json.RawMessage
		if e = d.Decode(&value); e != nil {
			return nil, e
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, errors.New("null project snapshot field")
		}
		values[key] = value
	}
	if _, e = d.Token(); e != nil {
		return nil, e
	}
	if _, e = d.Token(); e != io.EOF {
		return nil, errors.New("extra JSON value")
	}
	if len(values) != len(fields) {
		return nil, errors.New("unexpected project snapshot fields")
	}
	for _, key := range fields {
		if _, ok := values[key]; !ok {
			return nil, errors.New("missing project snapshot field")
		}
	}
	if e = json.Unmarshal(raw, out); e != nil {
		return nil, e
	}
	return values, nil
}

var projectUTCFormat = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,9})?Z$`)

func projectUTCTime(raw json.RawMessage) (time.Time, error) {
	var value string
	if e := json.Unmarshal(raw, &value); e != nil {
		return time.Time{}, e
	}
	if !projectUTCFormat.MatchString(value) {
		return time.Time{}, errors.New("expected UTC RFC3339Nano")
	}
	t, e := time.Parse(time.RFC3339Nano, value)
	if e != nil || t.IsZero() {
		return time.Time{}, errors.New("invalid project timestamp")
	}
	return t, nil
}
func projectIdentifier(s string, max int) bool {
	return s != "" && strings.TrimSpace(s) == s && utf8.ValidString(s) && utf8.RuneCountInString(s) <= max
}
func validateProjectRow(p ProjectProjection) error {
	if p.Type != "project" || !projectIdentifier(p.TenantID, 26) || !projectIdentifier(p.SnapshotID, 128) || !projectIdentifier(p.ID, 32) {
		return errors.New("invalid project identity")
	}
	for _, f := range []struct {
		value string
		max   int
	}{{p.Name, 255}, {p.Customer, 255}, {p.CustomerID, 64}, {p.Contract, 64}, {p.ContractID, 64}, {p.Category, 255}, {p.Team, 128}, {p.Manager, 128}, {p.ManagerIdentityID, 128}, {p.OwnerOrgID, 64}, {p.OwnerIdentityID, 128}, {p.Status, 32}} {
		if !utf8.ValidString(f.value) || utf8.RuneCountInString(f.value) > f.max {
			return errors.New("invalid project string")
		}
	}
	if p.Status == "" || p.Progress < 0 || p.Progress > 100 || p.ServiceItems < 0 || p.ServiceItems > maxProjectServiceItems {
		return errors.New("invalid project metrics")
	}
	for _, at := range []time.Time{p.CreatedAt, p.UpdatedAt, p.SourceUpdatedAt} {
		if _, offset := at.Zone(); offset != 0 {
			return errors.New("project timestamps must be UTC")
		}
	}
	if p.CreatedAt.IsZero() || p.UpdatedAt.IsZero() || p.SourceUpdatedAt.IsZero() || p.CreatedAt.After(p.UpdatedAt) || p.SourceUpdatedAt.Before(p.UpdatedAt) {
		return errors.New("invalid project timestamp order")
	}
	return nil
}

func readProjectSnapshot(ctx context.Context, reader io.Reader, expectedTenant string, options ProjectProjectionOptions, sink ProjectSnapshotSink) (ProjectSnapshotSummary, error) {
	fail := func(err error) (ProjectSnapshotSummary, error) { return ProjectSnapshotSummary{}, err }
	o, e := options.normalized()
	if e != nil {
		return fail(e)
	}
	if reader == nil || sink == nil || !projectIdentifier(expectedTenant, 26) {
		return fail(errors.New("invalid project snapshot input"))
	}
	ctx, cancel := context.WithTimeout(ctx, o.HTTPTimeout)
	defer cancel()
	// Transport timeout remains the caller's responsibility for blocking readers.
	input := bufio.NewReaderSize(io.LimitReader(reader, int64(o.MaxBytes)+1), o.MaxLineBytes+1)
	totalBytes := 0
	line := func() ([]byte, error) {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		b, e := input.ReadSlice('\n')
		totalBytes += len(b)
		if ce := ctx.Err(); ce != nil {
			return nil, ce
		}
		if totalBytes > o.MaxBytes {
			return nil, errors.New("project snapshot byte limit")
		}
		if len(b) > o.MaxLineBytes || e == bufio.ErrBufferFull {
			return nil, errors.New("project snapshot line limit")
		}
		if e != nil {
			// EOF may carry a non-empty unterminated fragment. Preserve it so the
			// post-end check cannot mistake trailing bytes for a clean EOF.
			return b, e
		}
		if !utf8.Valid(b) {
			return nil, errors.New("invalid project snapshot UTF-8")
		}
		return b, nil
	}
	raw, e := line()
	if e != nil {
		return fail(e)
	}
	var header struct {
		Type             string    `json:"type"`
		SchemaVersion    int       `json:"schema_version"`
		TenantID         string    `json:"tenant_id"`
		SnapshotID       string    `json:"snapshot_id"`
		SourceSnapshotAt time.Time `json:"source_snapshot_at"`
	}
	fields, e := projectObject(raw, projectHeaderFields, &header)
	if e != nil {
		return fail(e)
	}
	at, e := projectUTCTime(fields["source_snapshot_at"])
	if e != nil {
		return fail(e)
	}
	if header.Type != "header" || header.SchemaVersion != 1 || header.TenantID != expectedTenant || !projectIdentifier(header.SnapshotID, 128) {
		return fail(errors.New("invalid project header"))
	}
	summary := ProjectSnapshotSummary{TenantID: expectedTenant, SnapshotID: header.SnapshotID, SourceSnapshotAt: at}
	hash := sha256.New()
	batch := make([]ProjectProjection, 0, 200)
	lastID := ""
	serviceItems := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if e := sink.Stage(ctx, batch); e != nil {
			return e
		}
		batch = make([]ProjectProjection, 0, 200)
		return nil
	}
	for {
		raw, e = line()
		if e != nil {
			return fail(e)
		}
		var kind struct {
			Type string `json:"type"`
		}
		if e = json.Unmarshal(raw, &kind); e != nil {
			return fail(e)
		}
		if kind.Type == "end" {
			var end struct {
				Type           string `json:"type"`
				SchemaVersion  int    `json:"schema_version"`
				TenantID       string `json:"tenant_id"`
				SnapshotID     string `json:"snapshot_id"`
				Total          int    `json:"total"`
				ChecksumSHA256 string `json:"checksum_sha256"`
			}
			if _, e = projectObject(raw, projectEndFields, &end); e != nil {
				return fail(e)
			}
			checksum := hex.EncodeToString(hash.Sum(nil))
			if end.SchemaVersion != 1 || end.TenantID != expectedTenant || end.SnapshotID != header.SnapshotID || end.Total != summary.Total || end.ChecksumSHA256 != checksum {
				return fail(errors.New("invalid project end summary"))
			}
			// Even a second newline or whitespace after end invalidates the response.
			extra, e := line()
			if e != io.EOF || len(extra) != 0 {
				return fail(errors.New("content after project end"))
			}
			if e = ctx.Err(); e != nil {
				return fail(e)
			}
			if e = flush(); e != nil {
				return fail(e)
			}
			if e = ctx.Err(); e != nil {
				return fail(e)
			}
			summary.ChecksumSHA256 = checksum
			summary.verified = true
			return summary, nil
		}
		var p ProjectProjection
		fields, e = projectObject(raw, projectRowFields, &p)
		if e != nil {
			return fail(e)
		}
		for _, key := range []string{"created_at", "updated_at", "source_updated_at"} {
			if _, e = projectUTCTime(fields[key]); e != nil {
				return fail(e)
			}
		}
		if e = validateProjectRow(p); e != nil {
			return fail(e)
		}
		if p.TenantID != expectedTenant || p.SnapshotID != header.SnapshotID || p.ID <= lastID {
			return fail(errors.New("conflicting project snapshot row"))
		}
		summary.Total++
		serviceItems += p.ServiceItems
		if summary.Total > o.MaxProjects || serviceItems > maxProjectServiceItems {
			return fail(errors.New("project snapshot count limit"))
		}
		if summary.SourceUpdatedAt == nil || p.SourceUpdatedAt.After(*summary.SourceUpdatedAt) {
			latest := p.SourceUpdatedAt
			summary.SourceUpdatedAt = &latest
		}
		lastID = p.ID
		_, _ = hash.Write(raw)
		batch = append(batch, p)
		if len(batch) == 200 {
			if e = flush(); e != nil {
				return fail(fmt.Errorf("stage project batch: %w", e))
			}
		}
	}
}
