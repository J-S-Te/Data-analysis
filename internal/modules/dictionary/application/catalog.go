// Package application 暴露指标字典读取用例。
package application

import (
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dictionary/domain"
	"gorm.io/gorm"
)

// CatalogService 拥有此子系统当前发布的版本化
// 字典，有意保持本地，而非跨系统静态数据辅助组件。
type CatalogService struct{ db *gorm.DB }

// NewCatalogService 构造指标字典应用服务。
func NewCatalogService(dbs ...*gorm.DB) *CatalogService {
	var db *gorm.DB
	if len(dbs) > 0 {
		db = dbs[0]
	}
	return &CatalogService{db: db}
}

// Get 返回已发布指标字典及当前看板能力说明。
// 参数：无。
// 返回值：独立字典视图；口径确认与实际数据能力分别说明。
func (service *CatalogService) Get() domain.Catalog {
	return domain.Catalog{
		Version: "2026-11-12", Source: "Data-analysis/数据看板与统计分析子系统·指标字典（模板）.md",
		Status: "BUILTIN_CATALOG", Note: "目录版本是内置口径发布日期；各指标version是租户定义版本。" +
			"状态‘已确认’表示定义口径；绑定表示已有对应计算能力，不保证来源当前可用或存在有效样本。" +
			"字典启用只维护治理状态，不改变原生看板调度、展示和权限。新增资格默认关闭，租户显式开关保留。" +
			"当前支持合同统计、冻结价格折扣、项目状态、确认实际人员和设备利用率、完整首轮终决通过率、" +
			"退回决定原因分布、累计净核销回款率和部门/人员/项目净确认成本；金额按原币种分列。" +
			"3.1仍未绑定：现有编制完成至签发P50不等同提交至签发，不能冒充该指标。" +
			"交付/项目周期、返工率、完整账龄分桶、开票未回款TOP10、部门/个人产值尚未具备完整字典计算出口；" +
			"客户投诉暂缓。历史缺价、缺首轮证据或缺容量保留未知，未采集不补零。", Metrics: builtinMetrics(),
	}
}

// ListForTenant 返回租户自定义指标；尚未配置时使用内置目录。

func metric(code, name, dashboard, definition, formula, source, period, status string) domain.Metric {
	return domain.Metric{Code: code, Name: name, Dashboard: dashboard, Definition: definition, Formula: formula, Source: source, Period: period, Status: status}
}

var publishedMetrics = []domain.Metric{
	metric("1.1", "签约金额", "合同看板", "统计周期内已生效合同的含税金额", "按生效日归属周期求和", "dim_contract.amount_minor", "月/季", "已确认"),
	metric("1.2", "签约金额趋势", "合同看板", "按月或季度展示签约金额变化", "按1.1结果按时间粒度分组", "dim_contract", "月/季", "已确认"),
	metric("1.3", "区域分布", "合同看板", "按客户所属区域统计合同金额与数量", "按region分组求和/计数", "dim_contract + dim_customer", "周期", "已确认"),
	metric("1.4", "商机→合同转化率", "合同看板", "已转合同商机数占有效商机数比例", "已转合同商机数 / 有效商机数", "dim_opportunity", "周期", "已确认"),
	metric("1.5", "折扣分析", "合同看板", "按冻结标准价可比基准统计普通生效合同折扣区间；缺基准单列未知", "成交金额 / 冻结标准金额；按低于80%、80%至90%、90%至100%、原价、高于原价分桶", "dashboard_metric_snapshot.contract_management.discount", "生效月/全部合同", "已确认"),
	metric("2.1", "项目状态分布", "项目看板", "按项目当前状态统计项目数量", "按status分组计数", "dim_project", "T+1", "已确认"),
	metric("2.2", "交付周期", "项目看板", "实际开始日至交付完成日的天数分布", "按团队/经理计算均值、P50、P90", "dim_project", "T+1", "已确认"),
	metric("2.3", "返工率", "项目看板", "报告退回与现场重测占交付项目比例", "返工项目数 / 已交付项目数", "项目交付事实", "月", "已确认"),
	metric("2.4", "人员利用率", "项目看板", "独立确认的项目实际分钟占明确容量分钟比例；缺容量保留未知", "已确认实际分钟 / 明确容量分钟；零分母为未知", "dashboard_metric_snapshot.project_management.person_utilization", "北京业务月", "已确认"),
	metric("2.5", "设备利用率", "项目看板", "独立确认的设备实际分钟占明确容量分钟比例；缺容量保留未知", "已确认实际分钟 / 明确容量分钟；零分母为未知", "dashboard_metric_snapshot.project_management.equipment_utilization", "北京业务月", "已确认"),
	metric("2.6", "项目周期", "项目看板", "合同签订日至项目归档日的端到端周期", "归档日 - 签订日", "dim_project + dim_contract", "项目", "已确认"),
	metric("3.1", "报告周期", "报告看板", "报告提交至签发的处理时长", "签发时间 - 提交时间", "报告流程事实", "月", "已确认"),
	metric("3.2", "首次通过率", "报告看板", "有完整逻辑首轮终决证据的报告直接通过比例；待审及历史首次未知单列", "首次通过数 / 有完整首轮通过或退回决定的报告数", "dashboard_metric_snapshot.project_management.report_review", "首次决定北京业务月", "已确认"),
	metric("3.3", "退回原因分布", "报告看板", "按原因统计真实退回决定次数；去重报告数单列", "按reason_code计数退回决定；逻辑报告按subject_id去重单列", "dashboard_metric_snapshot.project_management.report_return", "退回决定北京业务月", "已确认"),
	metric("3.4", "客户投诉统计", "报告看板", "指定周期内未关闭客户投诉数量", "按状态过滤计数", "客户服务事实", "月", "已确认"),
	metric("4.1", "回款率", "财务看板", "截至当前符合条件应收的累计净核销回款比例；本月登记收款另列", "按币种：累计净核销回款 / 原始确认应收；零分母为未知", "结算CurrencyTotals；仅RELEASED合同的CONFIRMED应收", "当前累计快照", "已确认"),
	metric("4.2", "应收账款账龄", "财务看板", "按逾期天数区间统计应收金额", "按账龄区间分组求和", "应收账款事实", "T+1", "已确认"),
	metric("4.3", "开票未回款TOP10", "财务看板", "开票后尚未回款的金额排名", "按未回款金额降序取前十", "发票与回款事实", "T+1", "已确认"),
	metric("4.4", "部门产值", "财务看板", "按部门统计项目产值或结算收入", "按角色口径汇总产值", "项目/结算事实", "月", "已确认"),
	metric("4.5", "个人产值", "财务看板", "按人员统计项目产值或结算收入", "按角色口径汇总产值", "项目/结算事实", "月", "已确认"),
	metric("4.6", "部门/个人成本", "财务看板", "按明确承担组织或归属人员统计独立复核的不含税净确认成本；个人成本不等同工资", "按冻结organization_id/identity_id及币种汇总确认成本与关联冲回", "dashboard_metric_snapshot.settlement.recognized_cost", "确认北京业务月", "已确认"),
	metric("4.7", "项目成本", "财务看板", "按项目统计独立复核的不含税净确认成本；报销净支出、预付及税额分列不重复累加", "按冻结project_id及币种汇总确认成本与关联冲回", "dashboard_metric_snapshot.settlement.recognized_cost", "确认北京业务月", "已确认"),
}
