package infrastructure

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
	"github.com/unified-identity-auth-platform/data-analysis/internal/standardfacts"
	"gorm.io/gorm"
)

// LatestBusiness 按明确来源读取当前租户财务或报告快照。
// 参数：ctx 为已授权上下文，tenant 为可信租户，section 为看板类别。
// 返回值：真实快照、未发布状态或来源错误；不跨来源回退。
func (repository *GORMRepository) LatestBusiness(ctx context.Context, tenant, section string) (domain.BusinessSnapshot, error) {
	if tenant == "" {
		return domain.BusinessSnapshot{}, errors.New("missing trusted business snapshot tenant")
	}
	if section == "finance" && repository.financeSource == "standard" {
		return repository.standardBusinessSnapshot(ctx, tenant)
	}
	expected := businessSnapshotSource(section)
	if expected == "" {
		return domain.BusinessSnapshot{}, errors.New("unsupported business snapshot section")
	}
	var result domain.BusinessSnapshot
	err := repository.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row struct{ Payload string }
		err := tx.Table("business_dashboard_snapshot").Select("payload").Where("tenant_id=? AND section=?", tenant, section).Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			result = unavailableBusinessSnapshot(tenant, expected, "尚未完成本节来源同步，请配置受控只读数据来源。")
			return nil
		}
		if err != nil {
			return err
		}
		if err = json.Unmarshal([]byte(row.Payload), &result); err != nil {
			return err
		}
		if result.TenantID != tenant || result.Metrics == nil || !result.Available || result.SnapshotAt == "" {
			return errors.New("invalid business statistics snapshot")
		}
		reader := *repository
		reader.db = tx
		return reader.attachBusinessFreshness(ctx, &result, expected)
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return result, err
}

// standardBusinessSnapshot 为独立标准财务发布快照附加新鲜度。
// 参数：ctx 为授权上下文，tenant 为可信租户。
// 返回值：独立发布结果；外部同步总开关不会改变标准来源的状态。
func (repository *GORMRepository) standardBusinessSnapshot(ctx context.Context, tenant string) (domain.BusinessSnapshot, error) {
	value, err := (&standardfacts.Store{DB: repository.db}).Read(ctx, tenant)
	if err != nil {
		return domain.BusinessSnapshot{}, err
	}
	if value.SourceCode != "" && value.SourceCode != standardfacts.SourceCode {
		return domain.BusinessSnapshot{}, errors.New("invalid standard finance snapshot source")
	}
	value.SourceCode = standardfacts.SourceCode
	if !value.Available {
		value.Freshness = unavailableBusinessSnapshot(tenant, standardfacts.SourceCode, value.Basis).Freshness
		return value, nil
	}
	value.Freshness = application.BuildFreshness(application.FreshnessInput{
		PersistedSnapshot: true, PersistedSnapshotAt: value.SnapshotAt,
		SourceUpdatedAt: application.ParseDashboardTimestamp(value.SourceUpdatedAt),
	}, time.Now().UTC())
	return value, nil
}

// businessSnapshotSource 返回分区允许的唯一外部来源。
// 参数：section 为报告或联动财务分区代码。
// 返回值：项目或结算来源代码；未知分区返回空。
func businessSnapshotSource(section string) string {
	switch section {
	case "report":
		return "project_management"
	case "finance":
		return "settlement"
	default:
		return ""
	}
}

// unavailableBusinessSnapshot 构造没有首次成功发布的明确未知状态。
// 参数：tenant 为可信租户，source 为明确来源，reason 为公开业务原因。
// 返回值：不可用快照，不将缺失数据解释为零。
func unavailableBusinessSnapshot(tenant, source, reason string) domain.BusinessSnapshot {
	freshness := application.BuildFreshness(application.FreshnessInput{}, time.Now().UTC())
	freshness.StaleReason = reason
	return domain.BusinessSnapshot{TenantID: tenant, SourceCode: source, Metrics: map[string]any{},
		MissingMetrics: []string{}, Basis: reason, Freshness: freshness}
}

type businessSyncState struct {
	Enabled       bool
	LastStatus    string
	LastSuccessAt *string
	LastError     string
}

// attachBusinessFreshness 在一致读取内为本节快照附加真实来源状态。
// 参数：ctx 为读取上下文，value 为本节已校验快照，expected 为唯一来源。
// 返回值：读取或来源绑定错误；旧快照保留，但失败与停用立即标陈旧。
func (repository *GORMRepository) attachBusinessFreshness(ctx context.Context, value *domain.BusinessSnapshot, expected string) error {
	now := time.Now().UTC()
	sourceAt := application.ParseDashboardTimestamp(value.SourceUpdatedAt)
	if value.SourceCode == "" {
		// 旧载荷没有来源绑定，不能借新来源记录证明历史数据成功同步。
		value.Freshness = repository.buildFreshness(application.FreshnessInput{
			PersistedSnapshot: true, PersistedSnapshotAt: value.SnapshotAt, SourceUpdatedAt: sourceAt,
		}, now)
		return nil
	}
	if value.SourceCode != expected {
		return errors.New("invalid business statistics snapshot source")
	}
	var state businessSyncState
	err := repository.db.WithContext(ctx).Table("sync_source").Select("enabled,last_status,last_success_at,last_error").
		Where("tenant_id=? AND subsystem_code=?", value.TenantID, expected).
		Order("updated_at DESC, id DESC").Take(&state).Error
	known := !errors.Is(err, gorm.ErrRecordNotFound)
	if err != nil && known {
		return err
	}
	// 来源可能同时同步项目投影与报告；本节快照写入时间才是本节最后成功水位。
	sectionAt := application.ParseDashboardTimestamp(value.SnapshotAt)
	value.Freshness = repository.buildFreshness(application.FreshnessInput{
		SourceUpdatedAt: sourceAt, LastSuccessAt: sectionAt, SyncStatus: state.LastStatus,
		SyncSourceKnown: known, SyncEnabled: state.Enabled,
	}, now)
	if !known {
		value.Freshness.SyncedAt = application.FormatDashboardTimestamp(sectionAt)
		value.Freshness.StaleReason = "本节保留旧快照，但来源同步记录缺失，无法确认当前状态。"
	} else if sectionAt == nil || sectionAt.After(now) {
		value.Freshness.Stale = true
		value.Freshness.StaleReason = "本节快照缺少有效成功时间，无法确认数据是否为最新。"
	} else if !value.Freshness.Stale && applicationTimestamp(state.LastSuccessAt) == nil {
		value.Freshness.Stale = true
		value.Freshness.StaleReason = "来源缺少持久成功凭据，本节仅保留已有快照。"
	} else if !value.Freshness.Stale && strings.TrimSpace(state.LastError) != "" {
		value.Freshness.Stale = true
		value.Freshness.StaleReason = "来源仍记录同步错误，本节展示已有快照，请核对来源状态。"
	}
	return nil
}
