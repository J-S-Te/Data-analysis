package importreport

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/unified-identity-auth-platform/data-analysis/internal/aggregation"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/apperror"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/response"
	"gorm.io/gorm"
)

var ErrScope = errors.New("history scope unavailable")

type HistoryPeriod struct {
	Period           string `json:"period"`
	Currency         string `json:"currency"`
	ImportMode       string `json:"import_mode"`
	ReportMode       string `json:"report_mode"`
	Status           string `json:"status"`
	ContractCount    int64  `json:"contract_count"`
	AmountMinor      int64  `json:"amount_minor"`
	SourceFactsCount int64  `json:"source_facts_count"`
}

type HistoryResult struct {
	Periods          []HistoryPeriod `json:"periods"`
	UnassignedCount  int64           `json:"unassigned_count"`
	Basis            string          `json:"basis"`
	SyncStatus       string          `json:"sync_status"`
	SyncedAt         *time.Time      `json:"synced_at"`
	SourceSnapshotID string          `json:"source_snapshot_id"`
	AggregationRunID string          `json:"aggregation_run_id"`
	Stale            bool            `json:"stale"`
	ErrorCode        string          `json:"error_code"`
}

// History 按可信主体范围读取历史原日期和分币种统计。
// 参数：ctx 必须含实际授权主体，db 为数析聚合库。
// 返回值：原月份、未归期和模式分栏；无授权或未知范围返回错误。
func History(ctx context.Context, db *gorm.DB) (HistoryResult, error) {
	result := HistoryResult{Periods: []HistoryPeriod{}, Basis: "历史导入按原业务日期和币种单列；不计普通签约或赢单。未归期不以导入日期替代；source_facts_count为实际源库接入数，其余仅API摘要，不代表签名核验。"}
	p, ok := auth.FromContext(ctx)
	if !ok || p.TenantID == "" {
		return result, ErrScope
	}
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var run aggregation.ImportReportRun
		markerErr := tx.Where("tenant_id=?", p.TenantID).Take(&run).Error
		if markerErr != nil && !errors.Is(markerErr, gorm.ErrRecordNotFound) {
			return markerErr
		}
		result.SyncStatus = run.LastStatus
		if result.SyncStatus == "" {
			result.SyncStatus = "UNKNOWN"
		}
		result.SyncedAt = run.LastSuccessAt
		result.SourceSnapshotID = run.PublishedSnapshotID
		result.AggregationRunID = run.PublishedRunID
		result.ErrorCode = run.LastError
		result.Stale = result.SyncStatus != "SUCCESS" || run.LastSuccessAt == nil || time.Since(*run.LastSuccessAt) > 48*time.Hour || run.LastSuccessAt.After(time.Now().UTC())
		// 合并已核验范围条件到同一RR事务，发布标记和分栏不可混代次。
		scoped, scopeErr := historyScopeQuery(tx.WithContext(ctx), p)
		if scopeErr != nil {
			return scopeErr
		}
		period := "COALESCE(SUBSTR(CAST(start_date AS CHAR),1,7),'UNASSIGNED')"
		selection := period + " period,currency,import_mode,report_mode,status," +
			"COUNT(*) contract_count,COALESCE(SUM(amount_minor),0) amount_minor," +
			"SUM(CASE WHEN source_facts_applied THEN 1 ELSE 0 END) source_facts_count"
		if err := scoped.Select(selection).Group("period,currency,import_mode,report_mode,status").
			Order("period DESC,currency,import_mode,report_mode,status").Scan(&result.Periods).Error; err != nil {
			return err
		}
		for _, row := range result.Periods {
			if row.Period == "UNASSIGNED" {
				result.UnassignedCount += row.ContractCount
			}
		}
		return nil
	}, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	return result, err
}

// HistoryHandler 提供会话授权后的历史分栏HTTP入口。
// 参数：db 为数析聚合库，外层必须使用现有会话/许可/看板权限中间件。
// 返回值：读取或拒绝当前主体范围的Gin处理器。
func HistoryHandler(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		value, err := History(c.Request.Context(), db)
		if errors.Is(err, ErrScope) {
			response.Error(c, apperror.New(403, "IMPORT_HISTORY_SCOPE_UNAVAILABLE", "当前数据范围无法读取历史统计"))
			return
		}
		if err != nil {
			response.Error(c, apperror.New(500, "IMPORT_HISTORY_FAILED", "历史统计读取失败"))
			return
		}
		response.OK(c, value)
	}
}

// StatusHandler 提供逐项精确版本的可信机器只读查询。
// 参数：db 为聚合库，verifier 为独立验证器，license 为既有读取许可检查。
// 返回值：不暴露合同/客户详情且不信任请求租户的处理器。
func StatusHandler(db *gorm.DB, verifier *Verifier, license func(context.Context) error) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			response.Error(c, apperror.New(401, "INVALID_REPORT_STATUS_MACHINE", "机器身份无效"))
			return
		}
		tenant, err := verifier.Verify(c.Request.Context(), strings.TrimPrefix(header, "Bearer "))
		if err != nil {
			response.Error(c, apperror.New(401, "INVALID_REPORT_STATUS_MACHINE", "机器身份无效"))
			return
		}
		if license != nil {
			if err = license(c.Request.Context()); err != nil {
				response.Error(c, apperror.New(503, "REPORT_STATUS_LICENSE_UNAVAILABLE", "统计读取许可暂不可用"))
				return
			}
		}
		item := c.Query("item_id")
		revision, e1 := strconv.ParseUint(c.Query("source_revision"), 10, 64)
		version, e2 := strconv.ParseUint(c.Query("report_release_version"), 10, 64)
		if item == "" || len(item) > 64 || e1 != nil || e2 != nil || revision == 0 || version == 0 {
			response.Error(c, apperror.New(400, "INVALID_REPORT_STATUS_QUERY", "需要有效的项、修订和发布版本"))
			return
		}
		status, err := aggregation.ReadImportReportStatus(c.Request.Context(), db, tenant, item, revision, version)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			response.Error(c, apperror.New(http.StatusNotFound, "REPORT_STATUS_NOT_FOUND", "当前精确版本尚未应用"))
			return
		}
		if err != nil {
			response.Error(c, apperror.New(503, "REPORT_STATUS_UNAVAILABLE", "统计应用状态暂不可用"))
			return
		}
		response.OK(c, status)
	}
}

// historyScopeQuery 将可信个人或组织范围应用于历史投影查询。
// 参数：db 为读取事务，p 为已校验的会话主体。
// 返回值：范围内查询；缺少范围身份或未知模式返回拒绝错误。
func historyScopeQuery(db *gorm.DB, p auth.Principal) (*gorm.DB, error) {
	q := db.Table("contract_import_history").Where("tenant_id=?", p.TenantID)
	switch p.ScopeMode {
	case "TENANT":
	case "ORG", "TEAM":
		ids := append(append([]string{}, p.OrganizationIDs...), p.TeamIDs...)
		valid := []string{}
		for _, id := range ids {
			if id != "" {
				valid = append(valid, id)
			}
		}
		if len(valid) == 0 {
			return nil, ErrScope
		}
		q = q.Where("owner_org_id IN ?", valid)
	case "SELF":
		if p.UserID == "" {
			return nil, ErrScope
		}
		q = q.Where("owner_user_id=?", p.UserID)
	default:
		return nil, ErrScope
	}

	return q, nil
}
