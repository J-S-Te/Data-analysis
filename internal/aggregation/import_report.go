package aggregation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ImportReportItem 是源系统明确放行的单个历史统计版本。
type ImportReportItem struct {
	ItemID               string  `json:"item_id"`
	SourceRevision       uint64  `json:"source_revision"`
	ReportReleaseVersion uint64  `json:"report_release_version"`
	RequiredChannels     string  `json:"required_channels"`
	ImportMode           string  `json:"import_mode"`
	ReportMode           string  `json:"report_mode"`
	ContractID           string  `json:"contract_id"`
	ContractVersion      uint64  `json:"contract_version"`
	Status               string  `json:"status"`
	StartDate            *string `json:"start_date"`
	EndDate              *string `json:"end_date"`
	OwnerUserID          string  `json:"owner_user_id"`
	OwnerOrgID           string  `json:"owner_org_id"`
	CRMCustomerID        *uint64 `json:"crm_customer_id"`
	OpportunityID        *string `json:"opportunity_id"`
	AmountMinor          int64   `json:"amount_minor"`
	Currency             string  `json:"currency"`
	CreatedAt            string  `json:"created_at"`
}

type ImportReportSnapshot struct {
	SchemaVersion    int                `json:"schema_version"`
	TenantID         string             `json:"tenant_id"`
	SourceSnapshotID string             `json:"source_snapshot_id"`
	Complete         bool               `json:"complete"`
	Items            []ImportReportItem `json:"items"`
}

type ImportReportRun struct {
	TenantID            string `gorm:"primaryKey"`
	Generation          uint64
	LastStatus          string
	LastError           string
	LastRunAt           *time.Time
	LastSuccessAt       *time.Time
	PublishedSnapshotID string
	PublishedRunID      string
}

// TableName 指定独立报告发布栅栏表。
// 参数：无。
// 返回值：版本化迁移中的表名。
func (ImportReportRun) TableName() string { return "contract_import_report_run" }

type ImportReportStatus struct {
	TenantID             string     `gorm:"primaryKey" json:"tenant_id"`
	ItemID               string     `gorm:"primaryKey" json:"item_id"`
	SourceRevision       uint64     `gorm:"primaryKey" json:"source_revision"`
	ReportReleaseVersion uint64     `gorm:"primaryKey" json:"report_release_version"`
	RequiredChannels     string     `json:"required_channels"`
	State                string     `json:"state"`
	SourceSnapshotID     string     `json:"source_snapshot_id"`
	AggregationRunID     string     `json:"aggregation_run_id"`
	AppliedAt            *time.Time `json:"applied_at"`
	ErrorCode            string     `json:"error_code"`
	PayloadDigest        string     `json:"-"`
	APIApplied           bool       `json:"-"`
	SourceFactsApplied   bool       `json:"source_facts_connected"`
	SchemaVersion        int        `gorm:"-" json:"schema_version"`
	AppliedChannels      []string   `gorm:"-" json:"applied_channels"`
}

// TableName 指定逐项、逐版本的应用凭据表。
// 参数：无。
// 返回值：版本化迁移中的表名。
func (ImportReportStatus) TableName() string { return "contract_import_report_status" }

type ImportHistory struct {
	TenantID             string `gorm:"primaryKey"`
	ItemID               string `gorm:"primaryKey"`
	SourceRevision       uint64
	ReportReleaseVersion uint64
	ContractID           string
	ImportMode           string
	ReportMode           string
	Status               string
	StartDate            *string
	EndDate              *string
	OwnerUserID          string
	OwnerOrgID           string
	AmountMinor          int64
	Currency             string
	PayloadJSON          string
	SourceSnapshotID     string
	AggregationRunID     string
	SourceFactsApplied   bool
}

// TableName 指定与普通经营事实隔离的历史投影。
// 参数：无。
// 返回值：版本化迁移中的表名。
func (ImportHistory) TableName() string { return "contract_import_history" }

// readImportReport 解码有界、完整且租户一致的历史摘要。
// 参数：reader 为源响应体，tenant 为可信机器绑定租户。
// 返回值：校验后的摘要；任何缺失、歧义或超限均返回错误。
func readImportReport(reader io.Reader, tenant string) (ImportReportSnapshot, error) {
	var envelope struct {
		Code string               `json:"code"`
		Data ImportReportSnapshot `json:"data"`
	}
	raw, err := io.ReadAll(io.LimitReader(reader, 4<<20+1))
	if err != nil || len(raw) > 4<<20 {
		return envelope.Data, errors.New("import summary exceeds limit")
	}
	if err = json.Unmarshal(raw, &envelope); err != nil {
		return envelope.Data, errors.New("invalid import summary JSON")
	}
	s := envelope.Data
	if envelope.Code != "OK" {
		return s, errors.New("import summary rejected")
	}
	return s, validateImportReport(s, tenant)
}

// validateImportReport 限定首版历史分栏政策及精确发布版本。
// 参数：s 为完整摘要，tenant 为可信租户。
// 返回值：安全可发布时为nil，否则不允许写入任何投影。
func validateImportReport(s ImportReportSnapshot, tenant string) error {
	hash, err := hex.DecodeString(s.SourceSnapshotID)
	if err != nil || len(hash) != 32 || s.SchemaVersion != 1 || !s.Complete || s.TenantID != tenant || tenant == "" || len(s.Items) > 1000 || s.Items == nil {
		return errors.New("incomplete import summary")
	}
	seen := map[string]bool{}
	contracts := map[string]bool{}
	for _, v := range s.Items {
		if v.ItemID == "" ||
			len(v.ItemID) > 64 ||
			seen[v.ItemID] ||
			v.ContractID == "" ||
			len(v.ContractID) > 64 ||
			contracts[v.ContractID] ||
			v.SourceRevision == 0 ||
			v.ReportReleaseVersion == 0 ||
			v.ContractVersion == 0 {
			return errors.New("invalid import item identity")
		}
		seen[v.ItemID] = true
		contracts[v.ContractID] = true
		validMode := v.ImportMode == "ARCHIVE_ONLY" && v.ReportMode == "ARCHIVE_SEPARATE" || (v.ImportMode == "CONTINUE_LIVE" || v.ImportMode == "HISTORICAL_CLOSED") && (v.ReportMode == "HISTORICAL_SEPARATE" || v.ReportMode == "ARCHIVE_SEPARATE")
		if !validMode || (v.RequiredChannels != "API_ONLY" && v.RequiredChannels != "API_AND_SOURCE_FACTS") {
			return errors.New("unsupported import statistics policy")
		}
		if len(v.Currency) != 3 ||
			strings.Trim(v.Currency, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" ||
			len(v.OwnerUserID) > 64 ||
			len(v.OwnerOrgID) > 64 ||
			v.Status == "" ||
			len(v.Status) > 32 {
			return errors.New("invalid import statistics fields")
		}
		for _, date := range []*string{v.StartDate, v.EndDate} {
			if date != nil {
				if parsed, e := time.Parse("2006-01-02", *date); e != nil || parsed.Format("2006-01-02") != *date {
					return errors.New("invalid original business date")
				}
			}
		}
		if _, e := time.Parse(time.RFC3339Nano, v.CreatedAt); e != nil {
			return errors.New("invalid original creation timestamp")
		}
	}
	return nil
}

// beginImportReport 在网络请求前递增发布代次，阻止较早请求迟到覆盖。
// 参数：ctx 为许可和取消上下文。
// 返回值：本轮唯一代次；失败时不启动源读取。
func (r *Runner) beginImportReport(ctx context.Context) (uint64, error) {
	var run ImportReportRun
	err := r.aggregationWrite(ctx, func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&ImportReportRun{TenantID: r.tenantID}).Error; err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id=?", r.tenantID).Take(&run).Error; err != nil {
			return err
		}
		run.Generation++
		return tx.Model(&ImportReportRun{}).Where("tenant_id=?", r.tenantID).Updates(map[string]any{"generation": run.Generation, "last_status": "RUNNING", "last_error": "", "last_run_at": time.Now().UTC()}).Error
	})
	return run.Generation, err
}

// publishImportReport 同事务替换历史投影并保存逐项版本应用凭据。
// 参数：generation 为本轮代次，s 为API摘要，facts 为本轮源事实，sourceError 为脱敏失败码。
// 返回值：发布成功为nil；旧代次、同版本异内容及任何数据库失败回滚全部写入。
func (r *Runner) publishImportReport(ctx context.Context, generation uint64, s ImportReportSnapshot, facts map[string]ImportReportItem, sourceError string) error {
	if err := validateImportReport(s, r.tenantID); err != nil {
		return err
	}
	return r.aggregationWrite(ctx, func(tx *gorm.DB) error {
		var run ImportReportRun
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id=?", r.tenantID).Take(&run).Error; err != nil {
			return err
		}
		if run.Generation != generation {
			return errors.New("import report superseded")
		}
		if err := tx.Where("tenant_id=?", r.tenantID).Delete(&ImportHistory{}).Error; err != nil {
			return err
		}
		for _, item := range s.Items {
			if err := publishImportItem(tx, r.tenantID, generation, s.SourceSnapshotID, item, facts, sourceError); err != nil {
				return err
			}
		}
		var waiting int64
		runID := fmt.Sprintf("import-%d", generation)
		if err := tx.Model(&ImportReportStatus{}).Where("tenant_id=? AND aggregation_run_id=? AND state<>'APPLIED'", r.tenantID, runID).Count(&waiting).Error; err != nil {
			return err
		}
		status := "SUCCESS"
		if waiting > 0 {
			status = "WAITING_SOURCE"
		}
		if sourceError != "" && sourceError != "SOURCE_NOT_CONFIGURED" {
			status = "BLOCKED"
		}
		return tx.Model(&ImportReportRun{}).Where("tenant_id=? AND generation=?", r.tenantID, generation).Updates(map[string]any{"last_status": status, "last_error": sourceError, "last_success_at": time.Now().UTC(), "published_snapshot_id": s.SourceSnapshotID, "published_run_id": runID}).Error
	})
}

// publishImportItem 保存单项API投影及匹配的源事实证明。
// 参数：tx 为发布事务，tenant/generation/snapshot 为发布身份，item/facts 为同轮内容，sourceError 为失败码。
// 返回值：内容冲突或迟到修订返回错误，其余按实际通道保存状态。
func publishImportItem(tx *gorm.DB, tenant string, generation uint64, snapshot string, item ImportReportItem, facts map[string]ImportReportItem, sourceError string) error {
	encoded, _ := json.Marshal(item)
	sum := sha256.Sum256(encoded)
	digest := hex.EncodeToString(sum[:])
	var old ImportReportStatus
	q := tx.Where("tenant_id=? AND item_id=? AND source_revision=? AND report_release_version=?", tenant, item.ItemID, item.SourceRevision, item.ReportReleaseVersion)
	err := q.Take(&old).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if err == nil && old.PayloadDigest != digest {
		return errors.New("import item version content conflict")
	}
	var newer int64
	if err := tx.Model(&ImportReportStatus{}).Where("tenant_id=? AND item_id=? AND (source_revision>? OR report_release_version>?)", tenant, item.ItemID, item.SourceRevision, item.ReportReleaseVersion).Count(&newer).Error; err != nil {
		return err
	}
	if newer > 0 {
		return errors.New("import report revision regression")
	}
	now := time.Now().UTC()
	runID := fmt.Sprintf("import-%d", generation)
	st := ImportReportStatus{TenantID: tenant,
		ItemID:               item.ItemID,
		SourceRevision:       item.SourceRevision,
		ReportReleaseVersion: item.ReportReleaseVersion,
		RequiredChannels:     item.RequiredChannels,
		State:                "APPLIED",
		SourceSnapshotID:     snapshot,
		AggregationRunID:     runID,
		AppliedAt:            &now,
		PayloadDigest:        digest,
		APIApplied:           true,
	}
	fact, matched := facts[item.ItemID]
	if matched {
		matched = importItemsEqual(fact, item)
	}
	if item.RequiredChannels == "API_AND_SOURCE_FACTS" {
		st.SourceFactsApplied = matched
		if !st.SourceFactsApplied {
			st.State = "WAITING_SOURCE"
			st.ErrorCode = "SOURCE_FACTS_NOT_COVERED"
			if sourceError != "" {
				st.ErrorCode = sourceError
			}
			if sourceError != "" && sourceError != "SOURCE_NOT_CONFIGURED" {
				st.State = "BLOCKED"
			}
		}
	}
	h := ImportHistory{TenantID: tenant,
		ItemID:               item.ItemID,
		SourceRevision:       item.SourceRevision,
		ReportReleaseVersion: item.ReportReleaseVersion,
		ContractID:           item.ContractID,
		ImportMode:           item.ImportMode,
		ReportMode:           item.ReportMode,
		Status:               item.Status,
		StartDate:            item.StartDate,
		EndDate:              item.EndDate,
		OwnerUserID:          item.OwnerUserID,
		OwnerOrgID:           item.OwnerOrgID,
		AmountMinor:          item.AmountMinor,
		Currency:             item.Currency,
		PayloadJSON:          string(encoded),
		SourceSnapshotID:     snapshot,
		AggregationRunID:     runID,
		SourceFactsApplied:   st.SourceFactsApplied,
	}
	if err := tx.Create(&h).Error; err != nil {
		return err
	}
	return tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&st).Error
}

// ReadImportReportStatus 读取精确租户、项和版本的真实应用证明。
// 参数：ctx/db 为读取上下文与聚合库，其余参数为可信租户及请求的精确身份。
// 返回值：含实际已应用通道的状态；未发布返回记录不存在，不推断最大水位。
func ReadImportReportStatus(ctx context.Context, db *gorm.DB, tenant, item string, revision, version uint64) (ImportReportStatus, error) {
	var st ImportReportStatus
	err := db.WithContext(ctx).Where("tenant_id=? AND item_id=? AND source_revision=? AND report_release_version=?", tenant, item, revision, version).Take(&st).Error
	st.SchemaVersion = 1
	st.AppliedChannels = []string{}
	if st.APIApplied {
		st.AppliedChannels = append(st.AppliedChannels, "API_APPLIED")
	}
	if st.SourceFactsApplied {
		st.AppliedChannels = append(st.AppliedChannels, "SOURCE_FACTS_APPLIED")
	}
	return st, err
}

// SyncImportReport 获取获准摘要及本轮源事实并原子发布。
// 参数：ctx 为许可/取消上下文，api 为独立机器凭据同步器，url 为完整摘要URL。
// 返回值：任何源或发布失败均返回错误；API成功时仍如实发布等待/阻塞状态。
func (r *Runner) SyncImportReport(ctx context.Context, api *APISyncRunner, url string) (result error) {
	if api == nil || api.options.TenantID != r.tenantID || api.options.ContractCredential.Scope != "contract.import.report.read" {
		return errors.New("import report machine binding mismatch")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*api.options.HTTPTimeout)
	defer cancel()
	generation, err := r.beginImportReport(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if result != nil {
			failureCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer cancel()
			result = errors.Join(result, r.failImportReport(failureCtx, generation))
		}
	}()
	token, err := api.machineToken(ctx, api.options.ContractCredential)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: api.options.HTTPTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("import summary unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("import summary returned HTTP %d", response.StatusCode)
	}
	snapshot, err := readImportReport(response.Body, r.tenantID)
	if err != nil {
		return err
	}
	facts, sourceError := r.readImportSourceFacts(ctx, snapshot)
	if err = r.publishImportReport(ctx, generation, snapshot, facts, sourceError); err != nil {
		return err
	}
	if sourceError != "" && sourceError != "SOURCE_NOT_CONFIGURED" {
		return errors.New(sourceError)
	}
	return nil
}

// importItemsEqual 比较实际来源和API项，允许同一时刻的不同RFC3339时区表达。
// 参数：left/right 为同轮原始项，DATE字段仍按业务日期原样比较。
// 返回值：全部事实字段相同且创建时间语义相同时为true。
func importItemsEqual(left, right ImportReportItem) bool {
	leftTime, leftErr := time.Parse(time.RFC3339Nano, left.CreatedAt)
	rightTime, rightErr := time.Parse(time.RFC3339Nano, right.CreatedAt)
	if leftErr != nil || rightErr != nil {
		return false
	}
	left.CreatedAt = leftTime.UTC().Format(time.RFC3339Nano)
	right.CreatedAt = rightTime.UTC().Format(time.RFC3339Nano)
	a, _ := json.Marshal(left)
	b, _ := json.Marshal(right)
	return string(a) == string(b)
}

// failImportReport 记录当前代次失败，保留上次成功历史投影与时间。
// 参数：ctx 为有界失败收尾上下文，generation 为本轮发布栅栏。
// 返回值：写入错误；旧代次不覆盖新运行状态。
func (r *Runner) failImportReport(ctx context.Context, generation uint64) error {
	return r.aggregationWrite(ctx, func(tx *gorm.DB) error {
		return tx.Model(&ImportReportRun{}).Where("tenant_id=? AND generation=?", r.tenantID, generation).Updates(map[string]any{"last_status": "FAILED", "last_error": "SUMMARY_OR_SOURCE_UNAVAILABLE"}).Error
	})
}
