// Package admin 数据源与预警规则管理 API（设计方案 §9；骨架）。
package admin

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/apperror"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/response"
)

type SyncSource struct {
	ID            string  `gorm:"column:id;primaryKey" json:"id"`
	SubsystemCode string  `gorm:"column:subsystem_code" json:"subsystem_code"`
	DBHost        string  `gorm:"column:db_host" json:"db_host"`
	DBSchema      string  `gorm:"column:db_schema" json:"db_schema"`
	Enabled       bool    `gorm:"column:enabled" json:"enabled"`
	LastStatus    string  `gorm:"column:last_status" json:"last_status"`
	LastRunAt     *string `gorm:"column:last_run_at" json:"last_run_at"`
	LastError     *string `gorm:"column:last_error" json:"last_error"`
}

func (SyncSource) TableName() string { return "sync_source" }

type AlertRule struct {
	ID            string    `gorm:"column:id;primaryKey" json:"id"`
	RuleCode      string    `gorm:"column:rule_code" json:"rule_code"`
	Name          string    `gorm:"column:name" json:"name"`
	SourceFCT     string    `gorm:"column:source_fct" json:"source_fct"`
	Severity      string    `gorm:"column:severity" json:"severity"`
	Enabled       bool      `gorm:"column:enabled" json:"enabled"`
	ThresholdJSON *string   `gorm:"column:threshold_json" json:"threshold_json"`
	UpdatedBy     *string   `gorm:"column:updated_by" json:"updated_by"`
	CreatedAt     time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt     time.Time `gorm:"column:updated_at" json:"updated_at"`
}

func (AlertRule) TableName() string { return "alert_rule" }

type Handler struct{ db *gorm.DB }

func NewHandler(db *gorm.DB) *Handler { return &Handler{db: db} }

// ListSources 数据源状态（aggregation.manage）。
func (h *Handler) ListSources(c *gin.Context) {
	var sources []SyncSource
	if err := h.db.Order("subsystem_code").Find(&sources).Error; err != nil {
		response.Error(c, apperror.Wrap(err, http.StatusInternalServerError, "SOURCE_LIST_FAILED", "failed to list sources"))
		return
	}
	response.OK(c, sources)
}

type SyncJob struct {
	ID            string    `gorm:"column:id;primaryKey" json:"id"`
	SourceID      string    `gorm:"column:source_id" json:"source_id"`
	SubsystemCode string    `gorm:"column:subsystem_code" json:"subsystem_code"`
	Status        string    `gorm:"column:status" json:"status"`
	RequestedAt   time.Time `gorm:"column:requested_at" json:"requested_at"`
}

func (SyncJob) TableName() string { return "sync_job" }

// TriggerSource 将手工同步请求写入队列，由 aggregation-worker 异步执行。
func (h *Handler) TriggerSource(c *gin.Context, id string) {
	var source SyncSource
	if err := h.db.Where("id = ?", id).First(&source).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			response.Error(c, apperror.New(http.StatusNotFound, "SOURCE_NOT_FOUND", "sync source not found"))
			return
		}
		response.Error(c, apperror.Wrap(err, http.StatusInternalServerError, "SOURCE_LOOKUP_FAILED", "failed to load sync source"))
		return
	}
	if !source.Enabled {
		response.Error(c, apperror.New(http.StatusConflict, "SOURCE_DISABLED", "sync source is disabled"))
		return
	}
	now := time.Now().UTC()
	job := SyncJob{ID: newID(), SourceID: source.ID, SubsystemCode: source.SubsystemCode, Status: "QUEUED", RequestedAt: now}
	if err := h.db.Create(&job).Error; err != nil {
		response.Error(c, apperror.Wrap(err, http.StatusInternalServerError, "SYNC_JOB_CREATE_FAILED", "failed to queue sync job"))
		return
	}
	response.OK(c, gin.H{"id": job.ID, "source_id": source.ID, "status": job.Status, "note": "同步请求已排队，aggregation-worker 将异步执行"})
}

func newID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err == nil {
		return strings.TrimRight(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buffer), "=")[:26]
	}
	return strings.Repeat("0", 26-len(fmt.Sprint(time.Now().UnixNano()))) + fmt.Sprint(time.Now().UnixNano())
}

// ListAlertRules 预警规则（alert.manage）。
func (h *Handler) ListAlertRules(c *gin.Context) {
	var rules []AlertRule
	if err := h.db.Order("rule_code").Find(&rules).Error; err != nil {
		response.Error(c, apperror.Wrap(err, http.StatusInternalServerError, "RULE_LIST_FAILED", "failed to list rules"))
		return
	}
	response.OK(c, rules)
}

// PutAlertRules 更新预警规则（骨架：整表替换；阈值与平台 cfg 同步 TODO；审计 TODO）。
func (h *Handler) PutAlertRules(c *gin.Context) {
	var input []AlertRule
	if err := c.ShouldBindJSON(&input); err != nil {
		response.Error(c, apperror.New(http.StatusBadRequest, "RULE_PAYLOAD_INVALID", "invalid rule payload"))
		return
	}
	now := time.Now().UTC()
	actor := ""
	if principal, ok := auth.FromContext(c.Request.Context()); ok {
		actor = principal.UserID
	}
	rules := make([]AlertRule, 0, len(input))
	for _, rule := range input {
		if strings.TrimSpace(rule.RuleCode) == "" || strings.TrimSpace(rule.Name) == "" || strings.TrimSpace(rule.SourceFCT) == "" {
			response.Error(c, apperror.New(http.StatusBadRequest, "RULE_PAYLOAD_INVALID", "rule_code, name and source_fct are required"))
			return
		}
		if rule.ID == "" {
			rule.ID = newID()
		}
		rule.CreatedAt, rule.UpdatedAt = now, now
		if actor != "" {
			rule.UpdatedBy = &actor
		}
		rules = append(rules, rule)
	}
	if err := h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("1 = 1").Delete(&AlertRule{}).Error; err != nil {
			return err
		}
		return tx.Create(&rules).Error
	}); err != nil {
		response.Error(c, apperror.Wrap(err, http.StatusInternalServerError, "RULE_SAVE_FAILED", "failed to save rules"))
		return
	}
	response.OK(c, rules)
}
