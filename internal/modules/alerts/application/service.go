// Package application 实现告警中心用例并拥有存储
// 端口，有意不依赖 Gin 或 GORM。
package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/alerts/domain"
)

const (
	defaultPageSize = 50
	maxPageSize     = 100
)

var ErrInvalidQuery = errors.New("invalid alert query")

// AlertScope 是预警读取/状态变更的行级边界。TENANT 之外必须显式给出组织/团队
// 单元；未知模式按不可见处理，不能退化为全租户。
type AlertScope struct {
	Mode       string
	OrgUnitIDs []string
}

type ListQuery struct {
	Page     int
	PageSize int
	Severity string
	Status   string
	Keyword  string
	Scope    AlertScope
}

type ListResult struct {
	Items    []domain.Item `json:"items"`
	Page     int           `json:"page"`
	PageSize int           `json:"page_size"`
	Total    int64         `json:"total"`
}

// Repository 持久化租户范围内的告警状态。
type Repository interface {
	ListByTenant(context.Context, string, ListQuery) (ListResult, error)
	SummaryByTenant(context.Context, string, AlertScope) (domain.Summary, error)
	UpdateStatus(context.Context, string, string, string, AlertScope, time.Time, *time.Time) (bool, error)
}

// Service 协调告警读取及状态转换。
type Service struct {
	repository Repository
	now        func() time.Time
}

// NewService 构造生产告警服务。
func NewService(repository Repository) *Service {
	return NewServiceWithClock(repository, func() time.Time { return time.Now().UTC() })
}

// NewServiceWithClock 使用确定性时钟构造告警服务，
// 供单元测试使用。时钟仅属于本模块，不是跨系统辅助组件。
func NewServiceWithClock(repository Repository, now func() time.Time) *Service {
	return &Service{repository: repository, now: now}
}

// List 返回租户范围内的一页。筛选与排序在 SQL 中完成，
// 使调用方可访问第一页之后的告警，而非筛选
// 被截断的内存切片。
func (service *Service) List(ctx context.Context, tenantID string, query ListQuery) (ListResult, error) {
	query.Severity = strings.ToUpper(strings.TrimSpace(query.Severity))
	query.Status = strings.ToUpper(strings.TrimSpace(query.Status))
	query.Keyword = strings.TrimSpace(query.Keyword)
	if query.Page == 0 {
		query.Page = 1
	}
	if query.PageSize == 0 {
		query.PageSize = defaultPageSize
	}
	if query.Page < 1 || query.PageSize < 1 || query.PageSize > maxPageSize {
		return ListResult{}, fmt.Errorf("%w: page must be >=1 and page_size 1..%d", ErrInvalidQuery, maxPageSize)
	}
	if query.Severity != "" && query.Severity != "CRITICAL" && query.Severity != "HIGH" && query.Severity != "MEDIUM" && query.Severity != "LOW" {
		return ListResult{}, fmt.Errorf("%w: unsupported severity", ErrInvalidQuery)
	}
	if query.Status != "" && query.Status != "OPEN" && query.Status != "ACK" && query.Status != "CLOSED" {
		return ListResult{}, fmt.Errorf("%w: unsupported status", ErrInvalidQuery)
	}
	if len([]rune(query.Keyword)) > 100 {
		return ListResult{}, fmt.Errorf("%w: keyword exceeds 100 characters", ErrInvalidQuery)
	}
	return service.repository.ListByTenant(ctx, tenantID, query)
}

// Summary 返回当前租户及当前数据范围的预警总量、状态和严重度聚合。
func (service *Service) Summary(ctx context.Context, tenantID string, scope AlertScope) (domain.Summary, error) {
	return service.repository.SummaryByTenant(ctx, tenantID, scope)
}

// UpdateStatus 在租户边界内变更单个告警状态。调用方
// 无权查看或告警已不存在时返回 found=false。
// UpdateStatus 变更告警状态。
//
// AUD-IP-2026 / 批次 2：业务写入与审计事件同事务；平台不可用时由投递器重试，
// 不再出现"业务已提交、审计同步失败返 503"。
func (service *Service) UpdateStatus(ctx context.Context, tenantID, id, status string, scope AlertScope) (bool, error) {
	now := service.now()
	var closedAt *time.Time
	if status == "CLOSED" {
		closedAt = &now
	}
	return service.repository.UpdateStatus(ctx, id, tenantID, status, scope, now, closedAt)
}
