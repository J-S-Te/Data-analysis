// Package application exposes metric-dictionary read use cases.
package application

import (
	"context"
	"errors"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dictionary/domain"
	"gorm.io/gorm"
	"time"
)

// CatalogService owns the versioned dictionary currently published by this
// subsystem. It is deliberately local rather than a cross-system static-data helper.
type CatalogService struct{ db *gorm.DB }

// NewCatalogService constructs the metric dictionary application service.
func NewCatalogService(dbs ...*gorm.DB) *CatalogService {
	var db *gorm.DB
	if len(dbs) > 0 {
		db = dbs[0]
	}
	return &CatalogService{db: db}
}

// Get returns the published metric dictionary without exposing mutable storage.
func (service *CatalogService) Get() domain.Catalog {
	return domain.Catalog{
		Version: "2026-07-28", Source: "Data-analysis/数据看板与统计分析子系统·指标字典（模板）.md",
		Status: "ALL_CONFIRMED", Note: "指标口径只读；新增或修订需更新版本并通过发布评审", Metrics: publishedMetrics,
	}
}

// ListForTenant 返回租户自定义指标；尚未配置时使用内置目录。
func (service *CatalogService) ListForTenant(ctx context.Context, tenantID string) domain.Catalog {
	catalog := service.Get()
	if service.db == nil {
		return catalog
	}
	var rows []domain.Metric
	if service.db.WithContext(ctx).Table("metric_definition").Where("tenant_id = ?", tenantID).Order("code").Find(&rows).Error == nil && len(rows) > 0 {
		catalog.Metrics = rows
		catalog.Version = time.Now().UTC().Format("2006-01-02")
		catalog.Status = "TENANT_CUSTOM"
	}
	return catalog
}

// SaveTenantMetrics 原子替换租户指标定义，严格校验必填字段。
func (service *CatalogService) SaveTenantMetrics(ctx context.Context, tenantID, actorID string, metrics []domain.Metric) error {
	if service.db == nil || tenantID == "" || len(metrics) == 0 {
		return errors.New("metric definitions are not configured")
	}
	now := time.Now().UTC()
	return service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, metric := range metrics {
			if metric.Code == "" || metric.Name == "" || metric.Dashboard == "" || metric.Definition == "" || metric.Formula == "" || metric.Source == "" || metric.Period == "" {
				return errors.New("metric definition fields are required")
			}
			var id string
			tx.Table("metric_definition").Where("tenant_id = ? AND code = ?", tenantID, metric.Code).Pluck("id", &id)
			if id == "" {
				id = metric.Code + tenantID
			}
			if err := tx.Exec(`INSERT INTO metric_definition (id,tenant_id,code,name,dashboard,definition,formula,source,period,status,version,updated_by,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,1,?,?,?) ON DUPLICATE KEY UPDATE name=VALUES(name),dashboard=VALUES(dashboard),definition=VALUES(definition),formula=VALUES(formula),source=VALUES(source),period=VALUES(period),status=VALUES(status),version=version+1,updated_by=VALUES(updated_by),updated_at=VALUES(updated_at)`, id, tenantID, metric.Code, metric.Name, metric.Dashboard, metric.Definition, metric.Formula, metric.Source, metric.Period, metric.Status, actorID, now, now).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func metric(code, name, dashboard, definition, formula, source, period, status string) domain.Metric {
	return domain.Metric{Code: code, Name: name, Dashboard: dashboard, Definition: definition, Formula: formula, Source: source, Period: period, Status: status}
}

var publishedMetrics = []domain.Metric{
	metric("1.1", "签约金额", "合同看板", "统计周期内已生效合同的含税金额", "按生效日归属周期求和", "dim_contract.amount_minor", "月/季", "已确认"),
	metric("1.2", "签约金额趋势", "合同看板", "按月或季度展示签约金额变化", "按1.1结果按时间粒度分组", "dim_contract", "月/季", "已确认"),
	metric("1.3", "区域分布", "合同看板", "按客户所属区域统计合同金额与数量", "按region分组求和/计数", "dim_contract + dim_customer", "周期", "已确认"),
	metric("1.4", "商机→合同转化率", "合同看板", "已转合同商机数占有效商机数比例", "已转合同商机数 / 有效商机数", "dim_opportunity", "周期", "已确认"),
	metric("1.5", "折扣分析", "合同看板", "按折扣区间统计合同数量", "按折扣率区间分组计数", "合同服务项明细", "周期", "已确认"),
	metric("2.1", "项目状态分布", "项目看板", "按项目当前状态统计项目数量", "按status分组计数", "dim_project", "T+1", "已确认"),
	metric("2.2", "交付周期", "项目看板", "实际开始日至交付完成日的天数分布", "按团队/经理计算均值、P50、P90", "dim_project", "T+1", "已确认"),
	metric("2.3", "返工率", "项目看板", "报告退回与现场重测占交付项目比例", "返工项目数 / 已交付项目数", "项目交付事实", "月", "已确认"),
	metric("2.4", "人员利用率", "项目看板", "有效项目工时占可用工时比例", "有效工时 / 可用工时", "工时与排班事实", "月", "已确认"),
	metric("2.5", "设备利用率", "项目看板", "设备有效使用时长占可用时长比例", "有效使用时长 / 可用时长", "设备使用事实", "月", "已确认"),
	metric("2.6", "项目周期", "项目看板", "合同签订日至项目归档日的端到端周期", "归档日 - 签订日", "dim_project + dim_contract", "项目", "已确认"),
	metric("3.1", "报告周期", "报告看板", "报告提交至签发的处理时长", "签发时间 - 提交时间", "报告流程事实", "月", "已确认"),
	metric("3.2", "首次通过率", "报告看板", "首次审核直接通过的报告比例", "首次通过数 / 首次送审数", "报告审核事实", "月", "已确认"),
	metric("3.3", "退回原因分布", "报告看板", "按退回原因统计报告数量", "按reason分组计数", "报告审核事实", "周期", "已确认"),
	metric("3.4", "客户投诉统计", "报告看板", "指定周期内未关闭客户投诉数量", "按状态过滤计数", "客户服务事实", "月", "已确认"),
	metric("4.1", "回款率", "财务看板", "已回款金额占应回款金额比例", "已回款 / 应回款", "回款事实", "月", "已确认"),
	metric("4.2", "应收账款账龄", "财务看板", "按逾期天数区间统计应收金额", "按账龄区间分组求和", "应收账款事实", "T+1", "已确认"),
	metric("4.3", "开票未回款TOP10", "财务看板", "开票后尚未回款的金额排名", "按未回款金额降序取前十", "发票与回款事实", "T+1", "已确认"),
	metric("4.4", "部门产值", "财务看板", "按部门统计项目产值或结算收入", "按角色口径汇总产值", "项目/结算事实", "月", "已确认"),
	metric("4.5", "个人产值", "财务看板", "按人员统计项目产值或结算收入", "按角色口径汇总产值", "项目/结算事实", "月", "已确认"),
	metric("4.6", "部门/个人成本", "财务看板", "按部门或人员统计成本金额", "按角色口径汇总产值", "成本事实", "月", "已确认"),
	metric("4.7", "项目成本", "财务看板", "按项目统计对应支出与成本", "按project_id求和", "项目成本事实", "月", "已确认"),
}
