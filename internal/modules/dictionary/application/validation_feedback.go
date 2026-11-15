package application

// metricFieldFailure 用途：为既有指标校验拒绝附上真实表单字段。
// 参数：field 为字段名，rule 为现有校验条件，message 为安全中文指引。
// 返回值：兼容原错误码的结构化业务拒绝，不包含请求值。
func metricFieldFailure(field, rule, message string) error {
	return &Error{Code: "METRIC_VALIDATION_FAILED", Issues: []FieldIssue{
		{Field: field, Rule: rule, Message: message},
	}}
}
