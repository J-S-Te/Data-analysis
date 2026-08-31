package infrastructure

import (
	"testing"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
)

func TestMergeTrendPointsKeepsProjectOnlyMonthsAndAppliesLimitAfterMerge(t *testing.T) {
	rows := mergeTrendPoints(
		[]domain.TrendPoint{{Period: "2026-08", ContractAmountMinor: 100}},
		map[string]int{"2026-09": 4, "2026-08": 3, "2026-07": 2},
		2,
	)
	if len(rows) != 2 || rows[0].Period != "2026-09" || rows[0].ProjectCount != 4 || rows[1].Period != "2026-08" || rows[1].ProjectCount != 3 {
		t.Fatalf("merged trend = %#v, want latest project-only and shared months", rows)
	}
}
