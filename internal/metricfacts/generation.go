package metricfacts

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// GenerationRow 保存流式接收状态；只有 READY 代次能被发布。
type GenerationRow struct {
	ID           string `gorm:"primaryKey;size:32"`
	TenantID     string `gorm:"size:64;index:idx_metric_generation_age,priority:1"`
	SourceCode   string `gorm:"size:32"`
	State        string `gorm:"size:16"`
	RowCount     int64
	SHA256       string    `gorm:"size:64"`
	MetadataJSON string    `gorm:"type:json"`
	CreatedAt    time.Time `gorm:"precision:3;index:idx_metric_generation_age,priority:2"`
}

// TableName 指定完整代次的元数据表。
// 参数：无。
// 返回值：固定表名。
func (GenerationRow) TableName() string { return "dashboard_metric_generation" }

// FactRow 保存校验后的最小事实投影，唯一键负责跨块去重。
type FactRow struct {
	GenerationID         string `gorm:"primaryKey;size:32;uniqueIndex:uq_metric_fact_identity,priority:1;index:idx_metric_fact_period,priority:1;index:idx_metric_fact_org,priority:1;index:idx_metric_fact_identity,priority:1"`
	Sequence             int64  `gorm:"primaryKey"`
	TenantID             string `gorm:"size:64"`
	SourceCode           string `gorm:"size:32"`
	Kind                 string `gorm:"size:32;uniqueIndex:uq_metric_fact_identity,priority:2"`
	RecordID             string `gorm:"size:256;uniqueIndex:uq_metric_fact_identity,priority:3"`
	SubjectID            string `gorm:"size:256"`
	Period               string `gorm:"size:7;index:idx_metric_fact_period,priority:2"`
	OrganizationID       string `gorm:"size:256;index:idx_metric_fact_org,priority:2"`
	IdentityID           string `gorm:"size:256;index:idx_metric_fact_identity,priority:2"`
	ProjectID            string `gorm:"size:256"`
	OrganizationName     string `gorm:"size:256"`
	IdentityName         string `gorm:"size:256"`
	ProjectName          string `gorm:"size:256"`
	Status               string `gorm:"size:32"`
	Currency             string `gorm:"size:3"`
	Bucket               string `gorm:"size:32"`
	ReasonCode           string `gorm:"size:64"`
	AmountMinor          *int64
	TaxMinor             *int64
	ActualMinutes        *int64
	CapacityMinutes      *int64
	PendingCount         *int64
	UnknownCount         *int64
	AutoConfirmedMinutes int64
	AutoConfirmedCount   int64
	PayloadJSON          string `gorm:"type:json"`
}

// TableName 指定有界分块写入、按范围查询的事实表。
// 参数：无。
// 返回值：固定表名。
func (FactRow) TableName() string { return "dashboard_metric_fact" }

// Staged 是已收到结束凭据和 EOF 的候选代次；身份不由外部调用者填写。
type Staged struct{ id, tenant, source string }

// WriteTransaction 复用聚合器现有许可与队列租约围栏。
type WriteTransaction func(func(*gorm.DB) error) error

// StageStream 分块验证并暂存流，绝不修改当前已发布快照。
// 参数：reader 为机器来源，tenant/source/now 为可信边界，write 为受控短事务。
// 返回值：完整候选代次；格式、重复事实、取消或存储失败时整体拒绝发布。
func StageStream(reader io.Reader, tenant, source string, now time.Time, write WriteTransaction) (*Staged, error) {
	if write == nil || SourceKey(source) == "" {
		return nil, ErrInvalid
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return nil, err
	}
	staged := &Staged{id: hex.EncodeToString(random), tenant: tenant, source: source}
	receiver := &streamStager{staged: staged, write: write, now: now}
	metadata, count, digest, err := readStream(reader, tenant, source, now, receiver.begin, receiver.append)
	if err == nil {
		err = receiver.ready(metadata, count, digest)
	}
	if err != nil {
		discardErr := write(func(tx *gorm.DB) error { return Discard(tx, staged) })
		return nil, errors.Join(err, discardErr)
	}
	return staged, nil
}

type streamStager struct {
	staged *Staged
	write  WriteTransaction
	now    time.Time
	count  int64
}

// begin 建立本次独立暂存代次，清理同租户已经失效的一小块旧事实。
// 参数：metadata 为已通过来源和时间校验的空集合元数据。
// 返回值：围栏、清理或创建失败时返回错误。
func (s *streamStager) begin(metadata Bundle) error {
	raw, _ := json.Marshal(metadata)
	return s.write(func(tx *gorm.DB) error {
		if err := cleanupExpiredGeneration(tx, s.staged.tenant, s.now); err != nil {
			return err
		}
		return tx.Create(&GenerationRow{ID: s.staged.id, TenantID: s.staged.tenant, SourceCode: s.staged.source,
			State: "STAGING", MetadataJSON: string(raw), CreatedAt: s.now.UTC()}).Error
	})
}

// append 将一块事实与接收计数在同一短事务提交，唯一键拒绝跨块重复。
// 参数：rows 为不超过 256 行的已校验事实。
// 返回值：租约、唯一性、计数代次或数据库错误。
func (s *streamStager) append(rows []Row) error {
	values := make([]FactRow, 0, len(rows))
	for _, row := range rows {
		values = append(values, projectFact(s.staged, s.count+int64(len(values))+1, row))
	}
	err := s.write(func(tx *gorm.DB) error {
		if err := cleanupExpiredGeneration(tx, s.staged.tenant, s.now); err != nil {
			return err
		}
		inserted := tx.Create(&values)
		if inserted.Error != nil {
			return inserted.Error
		}
		if inserted.RowsAffected != int64(len(values)) {
			return ErrInvalid
		}
		result := tx.Model(&GenerationRow{}).Where("id=? AND state=? AND row_count=?", s.staged.id, "STAGING", s.count).
			Update("row_count", s.count+int64(len(values)))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("指标暂存代次已失效")
		}
		return nil
	})
	if err == nil {
		s.count += int64(len(rows))
	}
	return err
}

// ready 在收到正确结束凭据及 EOF 后记录可以发布的完整候选。
// 参数：metadata 为最终元数据，count/digest 为整个流的计数和摘要。
// 返回值：围栏、计数不一致或数据库错误。
func (s *streamStager) ready(metadata Bundle, count int64, digest string) error {
	raw, _ := json.Marshal(metadata)
	return s.write(func(tx *gorm.DB) error {
		result := tx.Model(&GenerationRow{}).Where("id=? AND state=? AND row_count=?", s.staged.id, "STAGING", count).
			Updates(map[string]any{"state": "READY", "sha256": digest, "metadata_json": string(raw)})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("指标暂存代次未完整结束")
		}
		return nil
	})
}

// Discard 将本次未发布候选标为可回收，绝不修改已经发布的快照。
// 参数：tx 为受原租约/许可保护的短事务，staged 为本次私有候选身份。
// 返回值：数据库错误；已被接管且围栏失效时由调用方保留错误并等待超期回收。
func Discard(tx *gorm.DB, staged *Staged) error {
	if staged == nil {
		return nil
	}
	return tx.Model(&GenerationRow{}).Where("id=? AND tenant_id=? AND source_code=?", staged.id, staged.tenant, staged.source).
		Where("state IN ?", []string{"STAGING", "READY"}).
		Where("NOT EXISTS (SELECT 1 FROM dashboard_metric_snapshot s WHERE s.generation_id=dashboard_metric_generation.id)").
		Update("state", "ABANDONED").Error
}

// projectFact 投影索引与汇总需要的列，同时保留经过校验的最小原始事实。
// 参数：staged 为候选身份，sequence 为连续行号，row 为已校验事实。
// 返回值：仅含当前租户/来源/代次的持久化行。
func projectFact(staged *Staged, sequence int64, row Row) FactRow {
	raw, _ := json.Marshal(row)
	return FactRow{GenerationID: staged.id, Sequence: sequence, TenantID: staged.tenant, SourceCode: staged.source,
		Kind: row.Kind, RecordID: row.RecordID, SubjectID: row.SubjectID, Period: row.Period,
		OrganizationID: row.OrganizationID, IdentityID: row.IdentityID, ProjectID: row.ProjectID,
		OrganizationName: row.OrganizationName, IdentityName: row.IdentityName, ProjectName: row.ProjectName,
		Status: row.Status, Currency: row.Currency, Bucket: row.Bucket, ReasonCode: row.ReasonCode,
		AmountMinor: row.AmountMinor, TaxMinor: row.TaxMinor, ActualMinutes: row.ActualMinutes,
		CapacityMinutes: row.CapacityMinutes, PendingCount: row.PendingCount, UnknownCount: row.UnknownCount,
		AutoConfirmedMinutes: row.AutoConfirmedMinutes, AutoConfirmedCount: row.AutoConfirmedCount,
		PayloadJSON: string(raw)}
}

// Publish 在调用者同一围栏事务内切换代次；调用者须同时发布来源成功水位。
// 参数：tx 为受许可/租约保护的事务，staged 为完整候选，now 为可信时间。
// 返回值：旧快照、候选失效或数据库错误；调用者必须回滚事务。
func Publish(tx *gorm.DB, staged *Staged, now time.Time) error {
	if staged == nil {
		return ErrInvalid
	}
	var generation GenerationRow
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id=? AND tenant_id=? AND source_code=? AND state=?", staged.id, staged.tenant, staged.source, "READY").
		Take(&generation).Error; err != nil {
		return err
	}
	var metadata Bundle
	if strictJSON([]byte(generation.MetadataJSON), &metadata) != nil || len(metadata.Rows) != 0 ||
		Validate(metadata, staged.tenant, staged.source, now) != nil {
		return ErrInvalid
	}
	if err := Save(tx, metadata, now); err != nil {
		return err
	}
	// 旧读者只认识payload_json；null行使旧v1校验明确失败，不能把元数据当可用零值。
	legacyMarker := metadata
	legacyMarker.Rows = nil
	marker, err := json.Marshal(legacyMarker)
	if err != nil {
		return err
	}
	if err := tx.Model(&SnapshotRow{}).Where("tenant_id=? AND source_code=?", staged.tenant, staged.source).
		Updates(map[string]any{"generation_id": staged.id, "payload_json": string(marker)}).Error; err != nil {
		return err
	}
	result := tx.Model(&GenerationRow{}).Where("id=? AND state=?", staged.id, "READY").Update("state", "PUBLISHED")
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrInvalid
	}
	return nil
}

// cleanupExpiredGeneration 有界清理已替换及失效代次，保留当前发布和未过期接收版本。
// 参数：tx 为当前聚合短事务，tenant 为受控租户，now 为可信时刻。
// 返回值：数据库错误；每次最多删除一个代次的 512 行，避免大清理长锁。
func cleanupExpiredGeneration(tx *gorm.DB, tenant string, now time.Time) error {
	var old GenerationRow
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id=?", tenant).
		Where("state IN ? OR created_at < ?", []string{"PUBLISHED", "DELETING", "ABANDONED"}, now.Add(-24*time.Hour)).
		Where("NOT EXISTS (SELECT 1 FROM dashboard_metric_snapshot s WHERE s.generation_id=dashboard_metric_generation.id)").
		Order("created_at,id").Take(&old).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := tx.Model(&GenerationRow{}).Where("id=?", old.ID).Update("state", "DELETING").Error; err != nil {
		return err
	}
	var first FactRow
	err = tx.Where("generation_id=?", old.ID).Order("sequence").Take(&first).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return tx.Where("id=?", old.ID).Delete(&GenerationRow{}).Error
	}
	if err != nil {
		return err
	}
	return tx.Where("generation_id=? AND sequence<=?", old.ID, first.Sequence+511).Delete(&FactRow{}).Error
}
