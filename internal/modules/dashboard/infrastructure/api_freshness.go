package infrastructure

import (
	"context"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
)

// apiSnapshotFreshness 将独立API来源状态与真实持久快照水位合并。
// 参数：ctx/tenant/source 为可信来源，snapshotAt 为本节已发布快照时间。
// 返回值：失败立即陈旧；兼容尚未登记独立API来源的历史快照。
func (repository *GORMRepository) apiSnapshotFreshness(
	ctx context.Context, tenant, source, snapshotAt string,
) domain.DataFreshness {
	input := application.FreshnessInput{
		PersistedSnapshot: true, PersistedSnapshotAt: snapshotAt,
	}
	watermark, known, err := repository.syncWatermark(ctx, tenant, source)
	if err != nil {
		return repository.buildFreshness(application.FreshnessInput{}, time.Now().UTC())
	}
	if known {
		input = application.FreshnessInput{
			LastSuccessAt: application.ParseDashboardTimestamp(snapshotAt),
			SyncStatus:    watermark.LastStatus, SyncSourceKnown: true,
			SyncEnabled: watermark.Enabled,
		}
	}
	return repository.buildFreshness(input, time.Now().UTC())
}
