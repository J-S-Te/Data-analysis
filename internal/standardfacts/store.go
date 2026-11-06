package standardfacts

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/auditoutbox"
	"github.com/unified-identity-auth-platform/data-analysis/internal/commercial"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

var ErrVersionConflict = errors.New("事实源版本已变化，请重新预览")
var ErrBatchConflict = errors.New("相同批次编号已用于不同内容，请使用新的批次编号")
var ErrLicenseDenied = errors.New("商业授权不允许发布标准财务事实")
var ErrInvalidPreview = errors.New("预览指纹不匹配，请重新预览原始文件")

// Store 仅持有数析自有聚合库，不访问其他业务库。
type Store struct {
	DB           *gorm.DB
	LicenseCheck commercial.Check
}

// PublishRequest 以预览指纹与乐观版本绑定待发布完整文件。
type PublishRequest struct {
	Document        Document `json:"document"`
	Digest          string   `json:"digest"`
	ExpectedVersion int64    `json:"expected_version"`
}

// Publication 是当前来源的服务端权威发布状态。
type Publication struct {
	Digest      string     `json:"digest"`
	SourceCode  string     `json:"source_code"`
	Published   bool       `json:"published"`
	Version     int64      `json:"version"`
	BatchID     string     `json:"batch_id"`
	PublishedAt *time.Time `json:"published_at"`
	Replayed    bool       `json:"replayed"`
}

type sourceRow struct {
	TenantID    string `gorm:"primaryKey;size:64"`
	SourceCode  string `gorm:"primaryKey;size:32"`
	Version     int64
	BatchID     string `gorm:"size:64"`
	PublishedAt *time.Time
}

// TableName 指定独立事实源发布指针表。
// 参数：无。
// 返回值：固定表名。
func (sourceRow) TableName() string { return "standard_finance_source" }

type batchRow struct {
	TenantID      string `gorm:"primaryKey;size:64"`
	SourceCode    string `gorm:"primaryKey;size:32"`
	BatchID       string `gorm:"primaryKey;size:64"`
	Version       int64
	SchemaVersion int
	Digest        string `gorm:"size:64"`
	AsOfDate      string `gorm:"size:10"`
	RowCount      int
	ActorID       string `gorm:"size:128"`
	PublishedAt   time.Time
	SnapshotJSON  string `gorm:"type:json"`
}

// TableName 指定不可变批次证据表。
// 参数：无。
// 返回值：固定表名。
func (batchRow) TableName() string { return "standard_finance_batch" }

type factRow struct {
	TenantID    string `gorm:"primaryKey;size:64"`
	SourceCode  string `gorm:"primaryKey;size:32"`
	BatchID     string `gorm:"primaryKey;size:64"`
	FactType    string `gorm:"primaryKey;size:16"`
	ExternalID  string `gorm:"primaryKey;size:64"`
	PayloadJSON string `gorm:"type:json"`
}

// TableName 指定带租户、来源及批次命名空间的事实表。
// 参数：无。
// 返回值：固定表名。
func (factRow) TableName() string { return "standard_finance_fact" }

// Status 读取当前租户的标准财务发布状态，不把查询故障当未发布。
// 参数：ctx 为请求上下文，tenant 为已认证租户。
// 返回值：权威版本、批次或存储错误。
func (s *Store) Status(ctx context.Context, tenant string) (Publication, error) {
	if tenant == "" {
		return Publication{}, errors.New("缺少已认证租户")
	}
	var row sourceRow
	err := s.database(ctx).Where("tenant_id=? AND source_code=?", tenant, SourceCode).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Publication{SourceCode: SourceCode}, nil
	}
	if err != nil {
		return Publication{}, err
	}
	if row.Version < 0 || (row.Version == 0 && row.BatchID != "") {
		return Publication{}, errors.New("标准财务发布指针损坏")
	}
	digest := ""
	if row.Version > 0 {
		var batch batchRow
		err := s.database(ctx).Select("digest").Where("tenant_id=? AND source_code=? AND batch_id=? AND version=?", tenant, SourceCode, row.BatchID, row.Version).Take(&batch).Error
		if err != nil {
			return Publication{}, err
		}
		if len(batch.Digest) != 64 {
			return Publication{}, errors.New("标准财务发布指纹损坏")
		}
		digest = batch.Digest
	}
	return Publication{SourceCode: SourceCode, Digest: digest, Published: row.Version > 0, Version: row.Version, BatchID: row.BatchID, PublishedAt: row.PublishedAt}, nil
}

// Preview 校验事实并绑定当前发布版本，不写入事实或发布指针。
// 参数：ctx 为请求上下文，tenant 为可信租户，doc 为上传事实，now 为当前时刻。
// 返回值：预览指标、指纹、期望版本或错误。
func (s *Store) Preview(ctx context.Context, tenant string, doc Document, now time.Time) (Preview, error) {
	preview, err := Analyze(doc, now)
	if err != nil {
		return Preview{}, err
	}
	status, err := s.Status(ctx, tenant)
	if err != nil {
		return Preview{}, err
	}
	preview.ExpectedVersion = status.Version
	return preview, nil
}

// Publish 再校验完整文件并原子发布事实、快照、版本指针和审计。
// 参数：ctx 为请求上下文，tenant/actor 为可信身份，requestID 为追踪号，request 为预览绑定，now 为时刻。
// 返回值：发布回执；重复内容重放原回执，冲突或依赖错误不产生部分写入。
func (s *Store) Publish(ctx context.Context, tenant, actor, requestID string, request PublishRequest, now time.Time) (Publication, error) {
	if tenant == "" || actor == "" || request.ExpectedVersion < 0 {
		return Publication{}, errors.New("缺少有效的会话或发布版本")
	}
	preview, err := Analyze(request.Document, now)
	if err != nil {
		return Publication{}, err
	}
	if preview.Digest != request.Digest {
		return Publication{}, ErrInvalidPreview
	}
	var result Publication
	err = s.database(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.LicenseCheck.Allow(ctx, commercial.MutateBusiness); err != nil {
			return ErrLicenseDenied
		}
		source := sourceRow{TenantID: tenant, SourceCode: SourceCode}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&source).Error; err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id=? AND source_code=?", tenant, SourceCode).Take(&source).Error; err != nil {
			return err
		}
		existing := batchRow{}
		err := tx.Where("tenant_id=? AND source_code=? AND batch_id=?", tenant, SourceCode, request.Document.BatchID).Take(&existing).Error
		if err == nil {
			if existing.Digest != preview.Digest {
				return ErrBatchConflict
			}
			result = Publication{SourceCode: SourceCode, Digest: existing.Digest, Published: true, Version: existing.Version, BatchID: existing.BatchID, PublishedAt: &existing.PublishedAt, Replayed: true}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if source.Version != request.ExpectedVersion {
			return ErrVersionConflict
		}
		version := source.Version + 1
		at := now.UTC()
		preview.Snapshot.TenantID = tenant
		preview.Snapshot.SnapshotAt = at.Format(time.RFC3339Nano)
		snapshot, err := json.Marshal(preview.Snapshot)
		if err != nil {
			return err
		}
		batch := batchRow{TenantID: tenant, SourceCode: SourceCode, BatchID: request.Document.BatchID, Version: version, SchemaVersion: 1, Digest: preview.Digest, AsOfDate: request.Document.AsOfDate, RowCount: preview.RowCount, ActorID: actor, PublishedAt: at, SnapshotJSON: string(snapshot)}
		if err := tx.Create(&batch).Error; err != nil {
			return err
		}
		if err := persistFacts(tx, tenant, request.Document); err != nil {
			return err
		}
		updated := tx.Model(&sourceRow{}).Where("tenant_id=? AND source_code=? AND version=?", tenant, SourceCode, source.Version).Updates(map[string]any{"version": version, "batch_id": batch.BatchID, "published_at": at})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return ErrVersionConflict
		}
		if err := auditoutbox.Append(tx, auditoutbox.Entry{TenantID: tenant, ActorID: actor, Action: "DATA_ANALYSIS:STANDARD_FINANCE_PUBLISH", ResourceType: "standard_finance_batch", ResourceID: batch.BatchID, RequestID: requestID}); err != nil {
			return err
		}
		result = Publication{SourceCode: SourceCode, Digest: preview.Digest, Published: true, Version: version, BatchID: batch.BatchID, PublishedAt: &at}
		return nil
	})
	return result, err
}

// persistFacts 仅向当前租户、来源、批次写入已校验事实，不覆盖联动表。
// 参数：tx 为发布事务，tenant 为可信租户，doc 为完整文件。
// 返回值：事实持久化错误。
func persistFacts(tx *gorm.DB, tenant string, doc Document) error {
	rows := make([]factRow, 0, len(doc.Receivables)+len(doc.Receipts))
	for _, value := range doc.Receivables {
		payload, err := json.Marshal(value)
		if err != nil {
			return err
		}
		rows = append(rows, factRow{tenant, SourceCode, doc.BatchID, "receivable", value.ExternalID, string(payload)})
	}
	for _, value := range doc.Receipts {
		payload, err := json.Marshal(value)
		if err != nil {
			return err
		}
		rows = append(rows, factRow{tenant, SourceCode, doc.BatchID, "receipt", value.ExternalID, string(payload)})
	}
	if len(rows) == 0 {
		return nil
	}
	return tx.CreateInBatches(rows, 100).Error
}

// Read 读取当前指针所指向的完整快照，不回退旧联动源或未发布事实。
// 参数：ctx 为请求上下文，tenant 为已授权租户。
// 返回值：当前快照、明确未发布状态或存储/损坏错误。
func (s *Store) Read(ctx context.Context, tenant string) (domain.BusinessSnapshot, error) {
	if tenant == "" {
		return domain.BusinessSnapshot{}, errors.New("缺少已认证租户")
	}
	var row batchRow
	err := s.database(ctx).Table("standard_finance_batch AS b").Select("b.*").Joins("JOIN standard_finance_source AS s ON s.tenant_id=b.tenant_id AND s.source_code=b.source_code AND s.batch_id=b.batch_id AND s.version=b.version").Where("b.tenant_id=? AND b.source_code=?", tenant, SourceCode).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		state, stateErr := s.Status(ctx, tenant)
		if stateErr != nil {
			return domain.BusinessSnapshot{}, stateErr
		}
		if state.Published {
			return domain.BusinessSnapshot{}, errors.New("标准财务发布指针损坏")
		}
		return domain.BusinessSnapshot{TenantID: tenant, Metrics: map[string]any{}, MissingMetrics: []string{}, Basis: "标准经营事实尚未发布，请由有权管理员上传、预览并明确发布完整财务事实。"}, nil
	}
	if err != nil {
		return domain.BusinessSnapshot{}, err
	}
	var snapshot domain.BusinessSnapshot
	if err := json.Unmarshal([]byte(row.SnapshotJSON), &snapshot); err != nil {
		return snapshot, err
	}
	if snapshot.TenantID != tenant || !snapshot.Available || snapshot.SnapshotAt == "" || snapshot.Metrics == nil {
		return domain.BusinessSnapshot{}, errors.New("标准财务发布快照损坏")
	}
	return snapshot, nil
}

// database 为事实持久化设置安全日志边界，避免驱动失败时打印完整财务事实。
// 参数：ctx 为当前请求上下文。
// 返回值：不打印原始业务 SQL 参数的独立数据库会话。
func (s *Store) database(ctx context.Context) *gorm.DB {
	return s.DB.WithContext(ctx).Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)})
}
