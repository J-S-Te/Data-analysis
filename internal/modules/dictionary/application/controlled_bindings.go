package application

// controlledBindings 标识已有计算出口且与发布定义一致的内置统计能力。
// 身份仅用于治理，不参与运行调度、授权或实时来源可用性判断。
// 3.1 的提交至签发与现有编制至签发P50不同，保持未绑定。
var controlledBindings = map[string]string{
	"1.1": "CONTRACT_SIGNING_FACTS_V1",
	"1.2": "CONTRACT_SIGNING_FACTS_V1",
	"1.3": "CONTRACT_REGION_FACTS_V1",
	"1.4": "CONTRACT_CONVERSION_FACTS_V1",
	"1.5": "CONTRACT_DISCOUNT_FACTS_V1",
	"2.1": "PROJECT_STATUS_COUNTS_V1",
	"2.4": "PERSON_UTILIZATION_FACTS_V1",
	"2.5": "EQUIPMENT_UTILIZATION_FACTS_V1",
	"3.2": "REPORT_FIRST_REVIEW_FACTS_V1",
	"3.3": "REPORT_RETURN_FACTS_V1",
	"4.1": "FINANCE_CURRENCY_TOTALS_V1",
	"4.6": "FINANCE_RECOGNIZED_COST_FACTS_V1",
	"4.7": "FINANCE_RECOGNIZED_COST_FACTS_V1",
}
