// Package standardfacts 提供独立于业务子系统的标准财务事实校验与原子发布。
package standardfacts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dashboard/domain"
)

const MaxFileBytes = 1 << 20
const MaxRows = 1000
const SourceCode = "standard-finance"

// 单条上限与1000条边界共同确保聚合整数低于浏览器安全整数上限。
const maxMinor int64 = 1_000_000_000_000

var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$`)
var currencyCode = regexp.MustCompile(`^[A-Z]{3}$`)

// Document 是一份截至业务日的完整财务事实，不接受客户端租户或计算后指标。
type Document struct {
	SchemaVersion int          `json:"schema_version"`
	BatchID       string       `json:"batch_id"`
	AsOfDate      string       `json:"as_of_date"`
	Complete      bool         `json:"complete"`
	Receivables   []Receivable `json:"receivables"`
	Receipts      []Receipt    `json:"receipts"`
}

// Receivable 是已确认应收的截至日余额事实，所有金额均为对应币种最小货币单位。
type Receivable struct {
	ExternalID     string `json:"external_id"`
	Currency       string `json:"currency"`
	OriginalMinor  int64  `json:"original_minor"`
	AllocatedMinor int64  `json:"allocated_minor"`
	DueDate        string `json:"due_date"`
}

// Receipt 是截至日已有效登记的回款事实，不等于收入确认。
type Receipt struct {
	ExternalID  string `json:"external_id"`
	Currency    string `json:"currency"`
	AmountMinor int64  `json:"amount_minor"`
	ReceiptDate string `json:"receipt_date"`
}

// CurrencyTotals 保持原生财务看板的分币种指标契约。
type CurrencyTotals struct {
	Currency           string   `json:"currency"`
	OriginalMinor      int64    `json:"original_minor"`
	AllocatedMinor     int64    `json:"allocated_minor"`
	OpenMinor          int64    `json:"open_minor"`
	Over90Minor        int64    `json:"over_90_minor"`
	MonthReceiptsMinor int64    `json:"month_receipts_minor"`
	CollectionRate     *float64 `json:"collection_rate"`
}

// Preview 是校验后的待发布摘要；摘要指纹绑定完整文件和顺序。
type Preview struct {
	SchemaVersion   int                     `json:"schema_version"`
	BatchID         string                  `json:"batch_id"`
	Digest          string                  `json:"digest"`
	RowCount        int                     `json:"row_count"`
	ExpectedVersion int64                   `json:"expected_version"`
	Snapshot        domain.BusinessSnapshot `json:"snapshot"`
}

// DecodeDocument 严格读取有界事实 JSON，拒绝未知字段、租户注入与尾随内容。
// 参数：reader 为上传内容，不包含 multipart 或外部引用。
// 返回值：标准文件或格式、大小错误。
func DecodeDocument(reader io.Reader) (Document, error) {
	var doc Document
	raw, err := io.ReadAll(io.LimitReader(reader, MaxFileBytes+1))
	if err != nil || len(raw) > MaxFileBytes {
		return doc, errors.New("事实文件不能超过 1 MiB")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&doc); err != nil {
		return doc, errors.New("事实文件格式不正确，请使用标准 JSON 模板，金额必须为整数")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return doc, errors.New("事实文件包含多余内容")
	}
	return doc, nil
}

// Analyze 校验完整事实并计算可复核的分币种财务指标。
// 参数：doc 为当前来源完整事实，now 为校验未来日期的服务端时刻。
// 返回值：预览及指纹；任一事实非法时不返回部分成功。
func Analyze(doc Document, now time.Time) (Preview, error) {
	if doc.SchemaVersion != 1 || !safeID.MatchString(doc.BatchID) || !doc.Complete {
		return Preview{}, errors.New("需要 schema_version=1、唯一批次编号及 complete=true 的完整事实文件")
	}
	asOf, err := time.Parse("2006-01-02", doc.AsOfDate)
	if err != nil || doc.AsOfDate > now.In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01-02") {
		return Preview{}, errors.New("截至日期须为有效业务日且不能晚于北京时间今天")
	}
	if doc.Receivables == nil || doc.Receipts == nil || len(doc.Receivables)+len(doc.Receipts) > MaxRows {
		return Preview{}, errors.New("应收和回款必须为数组，合计最多 1000 条；空数组代表已确认无记录")
	}
	rows, err := calculateTotals(doc, asOf)
	if err != nil {
		return Preview{}, err
	}
	encoded, err := json.Marshal(doc)
	if err != nil || len(encoded) > MaxFileBytes {
		return Preview{}, errors.New("事实文件无法编码或超过 1 MiB")
	}
	hash := sha256.Sum256(encoded)
	snapshot := domain.BusinessSnapshot{Available: true, Metrics: map[string]any{
		"currencies": rows, "month": doc.AsOfDate[:7], "as_of_date": doc.AsOfDate,
	}, MissingMetrics: []string{"recognized_income", "department_cost"},
		Basis: "标准经营事实导入：按截至日完整事实分币种计算，金额单位为分；已核销回款/原始应收为回款率；到期日早于截至日90天的未清余额计入账龄；本月回款不是收入确认。来源 standard-finance，批次 " + doc.BatchID + "，截至日 " + doc.AsOfDate + "。"}
	return Preview{SchemaVersion: 1, BatchID: doc.BatchID, Digest: hex.EncodeToString(hash[:]), RowCount: len(doc.Receivables) + len(doc.Receipts), Snapshot: snapshot}, nil
}

// calculateTotals 在校验各事实后以整数计算分币种指标，避免浮点金额误差。
// 参数：doc 为完整事实，asOf 为已校验截至业务日。
// 返回值：排序后的指标；重复编号、非法日期或金额返回错误。
func calculateTotals(doc Document, asOf time.Time) ([]CurrencyTotals, error) {
	totals := map[string]*CurrencyTotals{}
	seen := map[string]bool{}
	for i, row := range doc.Receivables {
		if !safeID.MatchString(row.ExternalID) || seen["r:"+row.ExternalID] || !currencyCode.MatchString(row.Currency) || row.OriginalMinor < 0 || row.OriginalMinor > maxMinor || row.AllocatedMinor < 0 || row.AllocatedMinor > row.OriginalMinor {
			return nil, fmt.Errorf("应收第 %d 条：编号须唯一，币种须为三位大写代码，整数金额须非负且核销不超过原额", i+1)
		}
		due, err := time.Parse("2006-01-02", row.DueDate)
		if err != nil {
			return nil, fmt.Errorf("应收第 %d 条：到期日必须为有效 YYYY-MM-DD 日期", i+1)
		}
		seen["r:"+row.ExternalID] = true
		total := totalFor(totals, row.Currency)
		total.OriginalMinor += row.OriginalMinor
		total.AllocatedMinor += row.AllocatedMinor
		open := row.OriginalMinor - row.AllocatedMinor
		total.OpenMinor += open
		if due.Before(asOf.AddDate(0, 0, -90)) {
			total.Over90Minor += open
		}
	}
	for i, row := range doc.Receipts {
		date, err := time.Parse("2006-01-02", row.ReceiptDate)
		if !safeID.MatchString(row.ExternalID) || seen["p:"+row.ExternalID] || !currencyCode.MatchString(row.Currency) || row.AmountMinor <= 0 || row.AmountMinor > maxMinor || err != nil || date.After(asOf) {
			return nil, fmt.Errorf("回款第 %d 条：编号须唯一、币种须合法、金额须为正整数、登记日不能晚于截至日", i+1)
		}
		seen["p:"+row.ExternalID] = true
		total := totalFor(totals, row.Currency)
		if row.ReceiptDate[:7] == doc.AsOfDate[:7] {
			total.MonthReceiptsMinor += row.AmountMinor
		}
	}
	result := make([]CurrencyTotals, 0, len(totals))
	for _, total := range totals {
		if total.OriginalMinor > 0 {
			rate := 100 * float64(total.AllocatedMinor) / float64(total.OriginalMinor)
			total.CollectionRate = &rate
		}
		result = append(result, *total)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Currency < result[j].Currency })
	return result, nil
}

// totalFor 获取当前币种的累计指标，不合并不同币种金额。
// 参数：totals 为币种索引，currency 为已校验币种代码。
// 返回值：可更新的该币种指标。
func totalFor(totals map[string]*CurrencyTotals, currency string) *CurrencyTotals {
	if totals[currency] == nil {
		totals[currency] = &CurrencyTotals{Currency: currency}
	}
	return totals[currency]
}
