package standardfacts

import (
	"bytes"
	"encoding/json"
	"errors"
)

// UnmarshalJSON 要求应收事实显式提供全部字段，不把缺失或null金额解释为零。
// 参数：data 为一条应收事实JSON。
// 返回值：缺字段、类型或未知字段错误。
func (r *Receivable) UnmarshalJSON(data []byte) error {
	type plain Receivable
	var value plain
	if err := decodeRequiredFact(data, &value, []string{"external_id", "currency", "original_minor", "allocated_minor", "due_date"}); err != nil {
		return err
	}
	*r = Receivable(value)
	return nil
}

// UnmarshalJSON 要求有效回款事实显式提供全部字段，不隐式补齐金额或日期。
// 参数：data 为一条有效回款事实JSON。
// 返回值：缺字段、类型或未知字段错误。
func (r *Receipt) UnmarshalJSON(data []byte) error {
	type plain Receipt
	var value plain
	if err := decodeRequiredFact(data, &value, []string{"external_id", "currency", "amount_minor", "receipt_date"}); err != nil {
		return err
	}
	*r = Receipt(value)
	return nil
}

// decodeRequiredFact 严格读取事实并逐一检查必填字段是否真实存在。
// 参数：data 为事实JSON，target 为不带自定义解码的目标，fields 为必填字段名。
// 返回值：缺失、null、类型或未知字段错误。
func decodeRequiredFact(data []byte, target any, fields []string) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for _, field := range fields {
		value, exists := raw[field]
		if !exists || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errors.New("事实字段必须全部填写，金额不得缺失或为null")
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}
