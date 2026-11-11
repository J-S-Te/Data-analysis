package metricfacts

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"strings"
	"time"
	"unicode"
)

const MaxBytes = 8 << 20
const MaxRows = 10000
const MaxInteger = int64(9007199254740991)

var ErrInvalid = errors.New("指标事实来源不完整或格式不正确")
var ErrScope = errors.New("缺少明确的指标数据范围")

// Decode 有界解码并完整验证来源租户、时间、事实身份和指标约束。
// 参数：reader为机器响应，tenant/source为可信预期，now为本次服务端时刻。
// 返回值：完整合法来源；失败不返回可发布的部分集合。
func Decode(reader io.Reader, tenant, source string, now time.Time) (Bundle, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, MaxBytes+1))
	if err != nil || len(raw) > MaxBytes {
		return Bundle{}, ErrInvalid
	}
	var envelope struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
		Data      Bundle `json:"data"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&envelope) != nil || envelope.Code != "OK" || decoder.Decode(new(any)) != io.EOF {
		return Bundle{}, ErrInvalid
	}
	if err := Validate(envelope.Data, tenant, source, now); err != nil {
		return Bundle{}, err
	}
	return envelope.Data, nil
}

// Validate 校验完整快照，防止持久化损坏或错误上游被解释成合法零值。
// 参数：bundle为候选集合，tenant/source/now为可信查询边界。
// 返回值：合法时nil；任意一行有错时整体拒绝。
func Validate(bundle Bundle, tenant, source string, now time.Time) error {
	if tenant == "" || bundle.TenantID != tenant || bundle.SourceCode != source ||
		bundle.SchemaVersion != 1 || !bundle.Complete || strings.TrimSpace(bundle.Basis) == "" ||
		bundle.Rows == nil || len(bundle.Rows) > MaxRows {
		return ErrInvalid
	}
	stamp, err := time.Parse(time.RFC3339Nano, bundle.SnapshotAt)
	if err != nil || stamp.After(now.Add(time.Minute)) || stamp.Unix() < 0 {
		return ErrInvalid
	}
	for _, value := range []string{bundle.SourceUpdatedAt, bundle.CollectionStartedAt} {
		if value != "" {
			parsed, err := time.Parse(time.RFC3339Nano, value)
			if err != nil || parsed.After(stamp.Add(time.Minute)) || parsed.Unix() < 0 {
				return ErrInvalid
			}
		}
	}
	seen := map[string]bool{}
	for _, row := range bundle.Rows {
		key := row.Kind + "\x00" + row.RecordID
		if !validText(row.RecordID, 256) || seen[key] || validateRow(row, source) != nil {
			return ErrInvalid
		}
		seen[key] = true
		if row.FirstDecidedAt != "" {
			decided, err := time.Parse(time.RFC3339Nano, row.FirstDecidedAt)
			if err != nil || decided.After(stamp.Add(time.Minute)) {
				return ErrInvalid
			}
		}
	}
	return nil
}

// validText 限制来源标识及显示标签，拒绝控制字符和不稳定空白。
// 参数：value为文本，limit为最大字节数。
// 返回值：文本非空、长度受控且无控制字符时为true。
func validText(value string, limit int) bool {
	return value != "" && len(value) <= limit && strings.TrimSpace(value) == value &&
		!strings.ContainsFunc(value, unicode.IsControl)
}

// ValidPeriod 验证可用于月度统计的严格北京业务月份。
// 参数：period为YYYY-MM文本。
// 返回值：合法月份时为true。
func ValidPeriod(period string) bool {
	parsed, err := time.Parse("2006-01", period)
	return err == nil && parsed.Format("2006-01") == period
}

// validCurrency 验证原币种编码，不进行隐式换汇。
// 参数：currency为来源声明的币种。
// 返回值：恰好三位大写ASCII字母时为true。
func validCurrency(currency string) bool {
	return len(currency) == 3 && strings.IndexFunc(currency, func(r rune) bool {
		return r < 'A' || r > 'Z'
	}) < 0
}

// validInteger 验证金额或分钟在浏览器可精确表达的范围内。
// 参数：value为必须存在的整数，signed表示是否允许真实冲回负数。
// 返回值：存在且范围合法时为true。
func validInteger(value *int64, signed bool) bool {
	return value != nil && *value <= MaxInteger && *value >= -MaxInteger && (signed || *value >= 0)
}

// validateRow 按来源和类型选择严格的事实校验，不允许跨系统伪造指标。
// 参数：row为来源行，source为已绑定的来源编码。
// 返回值：类型与完整性一致时nil，否则拒绝。
func validateRow(row Row, source string) error {
	for _, value := range []string{row.OrganizationID, row.IdentityID, row.ProjectID,
		row.OrganizationName, row.IdentityName, row.ProjectName} {
		if value != "" && !validText(value, 256) {
			return ErrInvalid
		}
	}
	if row.Period != "" && !ValidPeriod(row.Period) {
		return ErrInvalid
	}
	switch row.Kind {
	case "discount":
		if source == "contract_management" {
			return validateDiscount(row)
		}
	case "report_review", "report_return":
		if source == "project_management" {
			return validateReview(row)
		}
	case "person_utilization", "equipment_utilization":
		if source == "project_management" {
			return validateUtilization(row)
		}
	default:
		if source == "settlement" {
			return validateFinance(row)
		}
	}
	return ErrInvalid
}

// DiscountBucket 使用精确整数比价确定边界，八折属于80至90区间。
// 参数：actual为成交最小币种单位金额，standard为冻结可比标准额。
// 返回值：固定桶编码；非法金额返回空串。
func DiscountBucket(actual, standard int64) string {
	if actual < 0 || standard <= 0 || actual > MaxInteger || standard > MaxInteger {
		return ""
	}
	left := new(big.Int).Mul(big.NewInt(actual), big.NewInt(100))
	for _, boundary := range []struct {
		percent int64
		bucket  string
	}{
		{80, "LT_80"}, {90, "FROM_80_TO_90"}, {100, "FROM_90_TO_100"},
	} {
		right := new(big.Int).Mul(big.NewInt(standard), big.NewInt(boundary.percent))
		if left.Cmp(right) < 0 {
			return boundary.bucket
		}
	}
	if actual == standard {
		return "AT_100"
	}
	return "GT_100"
}

// validateDiscount 拒绝缺失基准、错桶和不可比金额冒充已知折扣。
// 参数：row为合同折扣事实。
// 返回值：已知或明确未知合法时nil，否则错误。
func validateDiscount(row Row) error {
	if row.Status == "UNKNOWN" {
		if row.Bucket == "" && validText(row.UnknownReason, 1024) &&
			(row.Currency == "" || validCurrency(row.Currency)) &&
			(row.AmountMinor == nil || validInteger(row.AmountMinor, false)) &&
			(row.StandardMinor == nil || validInteger(row.StandardMinor, false)) {
			return nil
		}
		return ErrInvalid
	}
	if row.Status != "KNOWN" || !validCurrency(row.Currency) ||
		!validInteger(row.AmountMinor, false) || !validInteger(row.StandardMinor, false) ||
		DiscountBucket(*row.AmountMinor, *row.StandardMinor) != row.Bucket || row.Bucket == "" {
		return ErrInvalid
	}
	return nil
}

// validateReview 验证首轮终决和真实退回身份，不把未知旧报告当失败。
// 参数：row为逻辑报告首轮或独立退回决定。
// 返回值：状态、时间及原因完整时nil，否则错误。
func validateReview(row Row) error {
	if row.Kind == "report_return" {
		if row.Status != "RETURNED" || !ValidPeriod(row.Period) ||
			!validText(row.SubjectID, 256) || !validText(row.ReasonCode, 64) {
			return ErrInvalid
		}
		return nil
	}
	if row.Status == "PENDING" || row.Status == "UNKNOWN" {
		if row.FirstDecidedAt == "" {
			return nil
		}
		return ErrInvalid
	}
	decided, err := time.Parse(time.RFC3339Nano, row.FirstDecidedAt)
	if err != nil || (row.Status != "PASSED" && row.Status != "RETURNED") ||
		decided.In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01") != row.Period {
		return ErrInvalid
	}
	return nil
}

// validateUtilization 验证明示容量与已确认实际分钟，未配置分母只能为零容量。
// 参数：row为单资源、月份、冻结组织汇总。
// 返回值：分钟和待审覆盖合法时nil，否则错误。
func validateUtilization(row Row) error {
	if row.Status != "CONFIRMED" || !ValidPeriod(row.Period) ||
		!validInteger(row.ActualMinutes, false) || !validInteger(row.CapacityMinutes, false) ||
		*row.ActualMinutes > *row.CapacityMinutes || !validInteger(row.PendingCount, false) ||
		!validInteger(row.UnknownCount, false) {
		return ErrInvalid
	}
	return nil
}

// validateFinance 区分已确认净额、现金和待审异常，不将待审计入确认总额。
// 参数：row为结算提供的最小确认或支出事实。
// 返回值：类型与状态及精确金额一致时nil，否则错误。
func validateFinance(row Row) error {
	if !ValidPeriod(row.Period) {
		return ErrInvalid
	}
	switch row.Kind {
	case "reimbursement_unknown":
		if row.Status == "UNKNOWN" && validText(row.UnknownReason, 1024) &&
			(row.Currency == "" || validCurrency(row.Currency)) &&
			(row.AmountMinor == nil || validInteger(row.AmountMinor, true)) {
			return nil
		}
	case "recognition_pending":
		if row.Status == "PENDING" {
			return nil
		}
	case "recognition_conflict":
		if row.Status == "CONFLICT" {
			return nil
		}
	case "recognized_income", "recognized_cost":
		if row.Status == "CONFIRMED" && validCurrency(row.Currency) &&
			validInteger(row.AmountMinor, true) && validInteger(row.TaxMinor, true) {
			return nil
		}
	case "reimbursement_spend", "prepayment_spend":
		if row.Status == "POSTED" && validCurrency(row.Currency) && validInteger(row.AmountMinor, true) {
			return nil
		}
	}
	return ErrInvalid
}
