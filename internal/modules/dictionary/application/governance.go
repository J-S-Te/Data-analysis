package application

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	mysqlDriver "github.com/go-sql-driver/mysql"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dictionary/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Error struct{ Code string }

func (e *Error) Error() string { return e.Code }
func fail(code string) error   { return &Error{code} }
func creationError(e error) error {
	if e == nil {
		return nil
	}
	var my *mysqlDriver.MySQLError
	if errors.Is(e, gorm.ErrDuplicatedKey) || (errors.As(e, &my) && my.Number == 1062) || strings.Contains(e.Error(), "UNIQUE constraint failed") {
		return fail("METRIC_CODE_CONFLICT")
	}
	return e
}
func validActor(actor string) error {
	if strings.TrimSpace(actor) == "" {
		return fail("METRIC_VALIDATION_FAILED")
	}
	return nil
}

type metricRow struct {
	domain.Metric `gorm:"embedded"`
	TenantID      string `gorm:"index:tenant_code,unique"`
	UpdatedBy     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	DeletedAt     *time.Time
}

func (metricRow) TableName() string { return "metric_definition" }

type versionRow struct {
	ID             string `gorm:"primaryKey"`
	TenantID       string `gorm:"index:metric_version,unique"`
	MetricID       string `gorm:"index:metric_version,unique"`
	Version        uint64 `gorm:"index:metric_version,unique"`
	MetricSnapshot string
	Operation      string
	ActorID        string
	CreatedAt      time.Time
}

func (versionRow) TableName() string { return "metric_definition_version" }

type referenceRow struct {
	ID            string `gorm:"primaryKey"`
	TenantID      string
	MetricCode    string
	ReferenceType string
	ReferenceID   string
}

func (referenceRow) TableName() string { return "metric_definition_reference" }

func newID() (string, error) {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		return "", e
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]), nil
}
func builtin(code string) (domain.Metric, bool) {
	for _, m := range builtinMetrics() {
		if m.Code == code {
			return m, true
		}
	}
	return domain.Metric{}, false
}
func builtinMetrics() []domain.Metric {
	out := append([]domain.Metric(nil), publishedMetrics...)
	for i := range out {
		digest := sha256.Sum256([]byte("data-analysis/builtin/" + out[i].Code))
		out[i].ID = base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:16])
		out[i].Version = 1
		out[i].Origin = "BUILTIN"
		out[i] = derived(out[i])
		out[i].Enabled = out[i].CanEnable
	}
	return out
}
func derived(m domain.Metric) domain.Metric {
	m.CalculationBinding = ""
	m.CanEnable = false
	// Project status counts are supplied by the native project snapshot reader.
	if m.Origin == "BUILTIN" && m.Code == "2.1" && m.Status == "已确认" {
		for _, b := range publishedMetrics {
			if b.Code == m.Code && sameCalculation(m, b) {
				m.CalculationBinding = "PROJECT_STATUS_COUNTS_V1"
				m.CanEnable = true
			}
		}
	}
	if !m.CanEnable {
		m.Enabled = false
	}
	return m
}
func sameCalculation(a, b domain.Metric) bool {
	return a.Dashboard == b.Dashboard && a.Definition == b.Definition && a.Formula == b.Formula && a.Source == b.Source && a.Period == b.Period
}

var codePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

func validate(m domain.Metric) error {
	if !codePattern.MatchString(m.Code) {
		return fail("METRIC_VALIDATION_FAILED")
	}
	for _, v := range []struct {
		s string
		n int
	}{{m.Name, 128}, {m.Dashboard, 128}, {m.Definition, 512}, {m.Formula, 512}, {m.Source, 255}, {m.Period, 64}, {m.Status, 32}} {
		if strings.TrimSpace(v.s) == "" || len([]rune(v.s)) > v.n {
			return fail("METRIC_VALIDATION_FAILED")
		}
	}
	return nil
}
func (s *CatalogService) ready(tenant string) error {
	if s.db == nil {
		return fail("METRIC_STORAGE_FAILED")
	}
	if strings.TrimSpace(tenant) == "" {
		return fail("METRIC_VALIDATION_FAILED")
	}
	return nil
}
func (s *CatalogService) ListForTenant(ctx context.Context, tenant string) (domain.Catalog, error) {
	c := s.Get()
	if e := s.ready(tenant); e != nil {
		return c, e
	}
	var rows []metricRow
	if e := s.db.WithContext(ctx).Where("tenant_id = ?", tenant).Limit(501).Find(&rows).Error; e != nil {
		return c, e
	}
	if len(rows) > 500 {
		return c, fail("METRIC_LIMIT_EXCEEDED")
	}
	c.Status = "TENANT_CATALOG"
	values := map[string]domain.Metric{}
	for _, m := range c.Metrics {
		values[m.Code] = m
	}
	for _, r := range rows {
		if r.DeletedAt != nil {
			delete(values, r.Code)
		} else {
			values[r.Code] = derived(r.Metric)
		}
	}
	c.Metrics = []domain.Metric{}
	for _, m := range values {
		c.Metrics = append(c.Metrics, m)
	}
	sort.Slice(c.Metrics, func(i, j int) bool { return c.Metrics[i].Code < c.Metrics[j].Code })
	return c, nil
}
func snapshot(tx *gorm.DB, r metricRow, actor, op string) error {
	id, e := newID()
	if e != nil {
		return e
	}
	b, e := json.Marshal(derived(r.Metric))
	if e != nil {
		return e
	}
	return tx.Create(&versionRow{ID: id, TenantID: r.TenantID, MetricID: r.ID, Version: r.Version, MetricSnapshot: string(b), Operation: op, ActorID: actor, CreatedAt: time.Now().UTC()}).Error
}
func load(tx *gorm.DB, tenant, code string) (metricRow, bool, error) {
	var r metricRow
	e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND code = ?", tenant, code).Take(&r).Error
	if e == nil {
		if r.DeletedAt != nil {
			return r, false, fail("METRIC_NOT_FOUND")
		}
		var count int64
		if e := tx.Model(&versionRow{}).Where("tenant_id = ? AND metric_id = ? AND version = ?", tenant, r.ID, r.Version).Count(&count).Error; e != nil {
			return r, false, e
		}
		if count == 0 {
			if e := snapshot(tx, r, r.UpdatedBy, "BASELINE"); e != nil {
				return r, false, e
			}
		}
		return r, true, nil
	}
	if !errors.Is(e, gorm.ErrRecordNotFound) {
		return r, false, e
	}
	if b, ok := builtin(code); ok {
		return metricRow{Metric: b, TenantID: tenant}, false, nil
	}
	return r, false, fail("METRIC_NOT_FOUND")
}
func save(tx *gorm.DB, r *metricRow, exists bool, version uint64, actor, op string) error {
	now := time.Now().UTC()
	r.UpdatedAt = now
	r.UpdatedBy = actor
	if exists {
		r.Version = version + 1
		res := tx.Model(&metricRow{}).Where("tenant_id = ? AND id = ? AND version = ? AND deleted_at IS NULL", r.TenantID, r.ID, version).Select("name", "dashboard", "definition", "formula", "source", "period", "status", "version", "enabled", "updated_by", "updated_at", "deleted_at").Updates(r)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fail("METRIC_VERSION_CONFLICT")
		}
	} else {
		id, e := newID()
		if e != nil {
			return e
		}
		r.ID = id
		r.CreatedAt = now
		if r.Origin == "BUILTIN" {
			b, _ := builtin(r.Code)
			baseline := metricRow{Metric: b, TenantID: r.TenantID}
			baseline.ID = id
			if e := snapshot(tx, baseline, actor, "BASELINE"); e != nil {
				return e
			}
		}
		r.Version = version + 1
		if e = tx.Create(r).Error; e != nil {
			return creationError(e)
		}
	}
	return snapshot(tx, *r, actor, op)
}
func (s *CatalogService) create(tx *gorm.DB, tenant, actor string, m domain.Metric) (domain.Metric, error) {
	if e := validActor(actor); e != nil {
		return m, e
	}
	if m.Status == "" {
		m.Status = "待确认"
	}
	if e := validate(m); e != nil {
		return m, e
	}
	if _, ok := builtin(m.Code); ok {
		return m, fail("METRIC_CODE_CONFLICT")
	}
	var n int64
	if e := tx.Model(&metricRow{}).Where("tenant_id = ? AND code = ?", tenant, m.Code).Count(&n).Error; e != nil {
		return m, e
	}
	if n > 0 {
		return m, fail("METRIC_CODE_CONFLICT")
	}
	if e := tx.Model(&metricRow{}).Where("tenant_id = ?", tenant).Count(&n).Error; e != nil {
		return m, e
	}
	if n >= 500 {
		return m, fail("METRIC_LIMIT_EXCEEDED")
	}
	m.ID = ""
	m.Version = 0
	m.Origin = "CUSTOM"
	m.Enabled = false
	m.CalculationBinding = ""
	m.CanEnable = false
	r := metricRow{Metric: m, TenantID: tenant}
	e := save(tx, &r, false, 0, actor, "CREATE")
	return derived(r.Metric), e
}
func (s *CatalogService) Create(ctx context.Context, tenant, actor string, m domain.Metric) (domain.Metric, error) {
	if e := s.ready(tenant); e != nil {
		return m, e
	}
	var out domain.Metric
	e := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error { var e error; out, e = s.create(tx, tenant, actor, m); return e })
	return out, e
}
func (s *CatalogService) update(tx *gorm.DB, tenant, actor, code string, m domain.Metric) (domain.Metric, error) {
	if e := validActor(actor); e != nil {
		return m, e
	}
	if m.Code != "" && m.Code != code {
		return m, fail("METRIC_VALIDATION_FAILED")
	}
	m.Code = code
	if e := validate(m); e != nil {
		return m, e
	}
	r, exists, e := load(tx, tenant, code)
	if e != nil {
		return m, e
	}
	if m.Version == 0 || m.Version != r.Version {
		return m, fail("METRIC_VERSION_CONFLICT")
	}
	m.ID = r.ID
	m.Origin = r.Origin
	m.Enabled = r.Enabled
	m = derived(m)
	r.Metric = m
	e = save(tx, &r, exists, m.Version, actor, "UPDATE")
	return derived(r.Metric), e
}
func (s *CatalogService) Update(ctx context.Context, tenant, actor, code string, m domain.Metric) (domain.Metric, error) {
	if e := s.ready(tenant); e != nil {
		return m, e
	}
	var out domain.Metric
	e := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error { var e error; out, e = s.update(tx, tenant, actor, code, m); return e })
	return out, e
}
func (s *CatalogService) SetEnabled(ctx context.Context, tenant, actor, code string, enabled bool, version uint64) (domain.Metric, error) {
	var out domain.Metric
	if e := validActor(actor); e != nil {
		return out, e
	}
	if e := s.ready(tenant); e != nil {
		return out, e
	}
	e := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		r, exists, e := load(tx, tenant, code)
		if e != nil {
			return e
		}
		if version == 0 || version != r.Version {
			return fail("METRIC_VERSION_CONFLICT")
		}
		r.Metric = derived(r.Metric)
		if enabled && !r.CanEnable {
			return fail("CALCULATION_BINDING_REQUIRED")
		}
		r.Enabled = enabled
		op := "DISABLE"
		if enabled {
			op = "ENABLE"
		}
		e = save(tx, &r, exists, version, actor, op)
		out = derived(r.Metric)
		return e
	})
	return out, e
}
func (s *CatalogService) Delete(ctx context.Context, tenant, actor, code string, version uint64) error {
	if e := validActor(actor); e != nil {
		return e
	}
	if e := s.ready(tenant); e != nil {
		return e
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		r, exists, e := load(tx, tenant, code)
		if e != nil {
			return e
		}
		if r.Origin == "BUILTIN" {
			return fail("METRIC_BUILTIN_PROTECTED")
		}
		if version == 0 || version != r.Version {
			return fail("METRIC_VERSION_CONFLICT")
		}
		var n int64
		if e := tx.Model(&referenceRow{}).Where("tenant_id = ? AND metric_code = ?", tenant, code).Count(&n).Error; e != nil {
			return e
		}
		if n > 0 {
			return fail("METRIC_REFERENCED")
		}
		now := time.Now().UTC()
		r.DeletedAt = &now
		r.Enabled = false
		return save(tx, &r, exists, version, actor, "DELETE")
	})
}
func (s *CatalogService) History(ctx context.Context, tenant, code string) ([]domain.MetricVersion, error) {
	out := []domain.MetricVersion{}
	if e := s.ready(tenant); e != nil {
		return out, e
	}
	var r metricRow
	e := s.db.WithContext(ctx).Where("tenant_id = ? AND code = ?", tenant, code).Take(&r).Error
	if errors.Is(e, gorm.ErrRecordNotFound) {
		if _, ok := builtin(code); ok {
			return out, nil
		}
		return out, fail("METRIC_NOT_FOUND")
	}
	if e != nil {
		return out, e
	}
	var rows []versionRow
	if e = s.db.WithContext(ctx).Where("tenant_id = ? AND metric_id = ?", tenant, r.ID).Order("version DESC").Limit(200).Find(&rows).Error; e != nil {
		return out, e
	}
	for _, v := range rows {
		var m domain.Metric
		if e = json.Unmarshal([]byte(v.MetricSnapshot), &m); e != nil {
			return out, e
		}
		out = append(out, domain.MetricVersion{Metric: m, Operation: v.Operation, ActorID: v.ActorID, CreatedAt: v.CreatedAt})
	}
	return out, nil
}

// Compatibility bulk writes are atomic upserts, never an implicit delete.
func (s *CatalogService) SaveTenantMetrics(ctx context.Context, tenant, actor string, metrics []domain.Metric) error {
	if e := s.ready(tenant); e != nil {
		return e
	}
	if len(metrics) == 0 || len(metrics) > 500 {
		return fail("METRIC_VALIDATION_FAILED")
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		seen := map[string]bool{}
		for _, m := range metrics {
			if seen[m.Code] {
				return fail("METRIC_VALIDATION_FAILED")
			}
			seen[m.Code] = true
			var e error
			if m.Version == 0 {
				_, e = s.create(tx, tenant, actor, m)
			} else {
				_, e = s.update(tx, tenant, actor, m.Code, m)
			}
			if e != nil {
				return e
			}
		}
		return nil
	})
}
