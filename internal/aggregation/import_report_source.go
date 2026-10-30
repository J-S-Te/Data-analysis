package aggregation

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

type importSourceRow struct {
	ID                           string
	Version                      uint64
	OriginImportItemID           string
	ImportSourceRevision         uint64
	ImportReportReleaseVersion   uint64
	ImportReportRequiredChannels string
	ImportMode                   string
	ImportPolicyJSON             string
	Status                       string
	StartDate                    *time.Time
	EndDate                      *time.Time
	OwnerUserID                  string
	OwnerOrgID                   *string
	CRMCustomerID                *uint64
	OpportunityID                *string
	AmountMinor                  int64
	Currency                     string
	CreatedAt                    time.Time
}

// readImportSourceFacts 从明确配置的合同只读通道读取本轮完整历史事实。
// 参数：ctx 为同步上下文，snapshot 为同轮API获准的项集合。
// 返回值：匹配的实际源行或脱敏错误码；缺DSN不降级也不连接其他来源。
func (r *Runner) readImportSourceFacts(ctx context.Context, snapshot ImportReportSnapshot) (map[string]ImportReportItem, string) {
	needed := false
	for _, item := range snapshot.Items {
		if item.RequiredChannels == "API_AND_SOURCE_FACTS" {
			needed = true
		}
	}
	if !needed {
		return nil, ""
	}
	dsn := r.sources["contract_management"]
	if dsn == "" {
		return nil, "SOURCE_NOT_CONFIGURED"
	}
	cfg, err := driver.ParseDSN(dsn)
	if err != nil {
		return nil, "SOURCE_UNAVAILABLE"
	}
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	limit := 10 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, "SOURCE_UNAVAILABLE"
		}
		if remaining < limit {
			limit = remaining
		}
	}
	for _, timeout := range []*time.Duration{&cfg.Timeout, &cfg.ReadTimeout, &cfg.WriteTimeout} {
		if *timeout <= 0 || *timeout > limit {
			*timeout = limit
		}
	}
	// 初始化阶段禁止使用Background探测服务器版本；首次联网由携带ctx的只读事务执行。
	src, err := gorm.Open(mysql.New(mysql.Config{DSN: cfg.FormatDSN(), SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		return nil, "SOURCE_UNAVAILABLE"
	}
	pool, err := src.DB()
	if err != nil {
		return nil, "SOURCE_UNAVAILABLE"
	}
	defer closeSourceConnectionPool(pool, "contract_import_report")
	facts, err := readImportFacts(ctx, src, r.tenantID, snapshot)
	if err != nil {
		return nil, "SOURCE_UNAVAILABLE"
	}
	return facts, ""
}

// readImportFacts 在单个只读RR快照内按API请求集合读取实际合同事实。
// 参数：ctx/db 为读取上下文与源连接，tenant 为可信租户，snapshot 为本轮摘要。
// 返回值：逐项源事实；任一读取失败不返回部分成功证明。
func readImportFacts(ctx context.Context, db *gorm.DB, tenant string, snapshot ImportReportSnapshot) (map[string]ImportReportItem, error) {
	facts := map[string]ImportReportItem{}
	ids := make([]string, 0, len(snapshot.Items))
	for _, item := range snapshot.Items {
		if item.RequiredChannels == "API_AND_SOURCE_FACTS" {
			ids = append(ids, item.ContractID)
		}
	}
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []importSourceRow
		if err := tx.Table("con_contract").Select("id,version,origin_import_item_id,import_source_revision,import_report_release_version,import_report_required_channels,import_mode,import_policy_json,status,start_date,end_date,owner_user_id,owner_org_id,crm_customer_id,opportunity_id,amount_minor,currency,created_at").Where("tenant_id=? AND origin_kind='IMPORTED' AND id IN ?", tenant, ids).Limit(1001).Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			var policy struct {
				ReportMode string `json:"report_mode"`
			}
			if err := json.Unmarshal([]byte(row.ImportPolicyJSON), &policy); err != nil {
				return err
			}
			org := ""
			if row.OwnerOrgID != nil {
				org = *row.OwnerOrgID
			}
			facts[row.OriginImportItemID] = ImportReportItem{ItemID: row.OriginImportItemID,
				SourceRevision:       row.ImportSourceRevision,
				ReportReleaseVersion: row.ImportReportReleaseVersion,
				RequiredChannels:     row.ImportReportRequiredChannels,
				ImportMode:           row.ImportMode,
				ReportMode:           policy.ReportMode,
				ContractID:           row.ID,
				ContractVersion:      row.Version,
				Status:               row.Status,
				StartDate:            importDate(row.StartDate),
				EndDate:              importDate(row.EndDate),
				OwnerUserID:          row.OwnerUserID,
				OwnerOrgID:           org,
				CRMCustomerID:        row.CRMCustomerID,
				OpportunityID:        row.OpportunityID,
				AmountMinor:          row.AmountMinor,
				Currency:             row.Currency,
				CreatedAt:            row.CreatedAt.UTC().Format(time.RFC3339Nano),
			}
		}
		return ctx.Err()
	}, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	return facts, err
}

// importDate 保留源DATE的业务年月日，不以同步时间补齐空日期。
// 参数：value 为数据库读取的可空日期。
// 返回值：原业务日期字符串，空值仍为空。
func importDate(value *time.Time) *string {
	if value == nil {
		return nil
	}
	date := value.Format("2006-01-02")
	return &date
}
