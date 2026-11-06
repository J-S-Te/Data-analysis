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

// projectOptionalRowFields 是允许出现在 project 行、但不要求出现的字段。
// 旧 PM 不发这些键时按“未采集”落空串；新 PM 发出时逐个校验。
var projectOptionalRowFields = strings.Fields("report_stage")

var projectEndFields = strings.Fields("type schema_version tenant_id snapshot_id total checksum_sha256")

// projectReportStages 是 PM 派生报告阶段的封闭词表。
// 空串表示“未进入报告段/不可判定”，与“无整改”以外的未知码一律拒绝。
var projectReportStages = map[string]bool{
	"": true, "NONE": true, "RECTIFYING": true, "COMPILING": true,
	"REVIEWED": true, "ISSUED": true, "ARCHIVED": true,
}

// validProjectReportStage 校验报告阶段取值：只接受封闭词表内的精确值，
// 因此前后空格、大小写变体、超长字符串都会被拒绝（fail-closed）。
func validProjectReportStage(value string) bool {
	return utf8.ValidString(value) && utf8.RuneCountInString(value) <= 32 && projectReportStages[value]
}

// validateProjectReportStage 校验原始 JSON 片段必须是字符串且为合法阶段码。
// 缺失的键不调用本函数：旧 PM 没有该字段是合法输入。
func validateProjectReportStage(raw json.RawMessage) error {
	var value string
	if e := json.Unmarshal(raw, &value); e != nil {
		return errors.New("invalid project report stage")
	}
	if !validProjectReportStage(value) {
		return errors.New("invalid project report stage")
	}
	return nil
}

// projectFieldSet 在初始化期把字段名固化成集合，逐行解析不再重复分配。
func projectFieldSet(required, optional []string) map[string]bool {
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, key := range required {
		allowed[key] = true
	}
	for _, key := range optional {
		allowed[key] = true
	}
	return allowed
}

var (
	projectHeaderAllowed = projectFieldSet(projectHeaderFields, nil)
	projectRowAllowed    = projectFieldSet(projectRowFields, projectOptionalRowFields)
	projectEndAllowed    = projectFieldSet(projectEndFields, nil)
)

// Decode an exact object, rejecting duplicate, missing, null and unknown fields.
// Optional fields may be absent but are still validated when present, so the
// contract stays “required 23 fields + at most one optional field”.
// encoding/json alone accepts duplicate keys and replaces invalid UTF-8.
func projectObject(raw []byte, fields []string, allowed map[string]bool, out any) (map[string]json.RawMessage, error) {
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
		if !allowed[key] {
			return nil, errors.New("unexpected project snapshot fields")
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
	if len(values) < len(fields) || len(values) > len(allowed) {
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
	// report_stage 是可选增量维度：空串=未采集，其余只接受封闭词表内的精确码。
	if !validProjectReportStage(p.ReportStage) {
		return errors.New("invalid project report stage")
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
	fields, e := projectObject(raw, projectHeaderFields, projectHeaderAllowed, &header)
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
			if _, e = projectObject(raw, projectEndFields, projectEndAllowed, &end); e != nil {
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
		fields, e = projectObject(raw, projectRowFields, projectRowAllowed, &p)
		if e != nil {
			return fail(e)
		}
		if rawStage, ok := fields["report_stage"]; ok {
			if e = validateProjectReportStage(rawStage); e != nil {
				return fail(e)
			}
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
