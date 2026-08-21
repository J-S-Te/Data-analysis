// Package dashboard 提供合同与项目快照的轻量查询接口。
//
// Metabase 仍负责完整图表，但这些接口为统一前端提供可靠的最新摘要，
// 避免某个 Metabase 卡片配置异常时页面只显示空白嵌入框。
package dashboard

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/apperror"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/response"
)

type contractSnapshot struct {
	Available         bool   `gorm:"-" json:"available"`
	TenantID          string `json:"tenant_id"`
	SnapshotAt        string `json:"snapshot_at"`
	TotalAmountMinor  int64  `json:"total_amount_minor"`
	TotalContracts    int64  `json:"total_contracts"`
	ApprovalContracts int64  `json:"approval_contracts"`
	ActiveContracts   int64  `json:"active_contracts"`
	ExpiredContracts  int64  `json:"expired_contracts"`
}

type projectSnapshot struct {
	Available        bool           `gorm:"-" json:"available"`
	TenantID         string         `json:"tenant_id"`
	SnapshotAt       string         `json:"snapshot_at"`
	ProjectCount     int            `json:"project_count"`
	InFlightProjects int            `json:"in_flight_projects"`
	RiskProjects     int            `json:"risk_projects"`
	ServiceItems     int            `json:"service_items"`
	StatusCounts     map[string]int `json:"status_counts"`
}

type Handler struct{ db *gorm.DB }

func NewHandler(db *gorm.DB) *Handler { return &Handler{db: db} }

func (h *Handler) Contract(c *gin.Context) {
	principal, ok := auth.FromContext(c.Request.Context())
	if !ok {
		response.Error(c, apperror.ErrUnauthenticated)
		return
	}
	var row contractSnapshot
	err := h.db.WithContext(c.Request.Context()).Table("api_contract_dashboard").
		Where("tenant_id = ?", principal.TenantID).Order("snapshot_at DESC").First(&row).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		response.Error(c, apperror.Wrap(err, http.StatusInternalServerError, "CONTRACT_DASHBOARD_FAILED", "failed to load contract dashboard"))
		return
	}
	if err == gorm.ErrRecordNotFound {
		row.TenantID = principal.TenantID
	} else {
		row.Available = true
	}
	response.OK(c, row)
}

func (h *Handler) Project(c *gin.Context) {
	principal, ok := auth.FromContext(c.Request.Context())
	if !ok {
		response.Error(c, apperror.ErrUnauthenticated)
		return
	}
	var row struct {
		projectSnapshot
		StatusCountsJSON string `gorm:"column:status_counts_json"`
	}
	err := h.db.WithContext(c.Request.Context()).Table("api_project_dashboard").
		Where("tenant_id = ?", principal.TenantID).Order("snapshot_at DESC").First(&row).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		response.Error(c, apperror.Wrap(err, http.StatusInternalServerError, "PROJECT_DASHBOARD_FAILED", "failed to load project dashboard"))
		return
	}
	row.StatusCounts = map[string]int{}
	if row.StatusCountsJSON != "" {
		if err := json.Unmarshal([]byte(row.StatusCountsJSON), &row.StatusCounts); err != nil {
			response.Error(c, apperror.Wrap(err, http.StatusInternalServerError, "PROJECT_DASHBOARD_INVALID", "invalid project dashboard snapshot"))
			return
		}
	}
	if err == gorm.ErrRecordNotFound {
		row.TenantID = principal.TenantID
	} else {
		row.Available = true
	}
	response.OK(c, row.projectSnapshot)
}
