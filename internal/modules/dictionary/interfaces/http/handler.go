package http

import (
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dictionary/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dictionary/domain"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/response"
)

type Handler struct{ catalog *application.CatalogService }

func NewHandler(catalog *application.CatalogService) *Handler { return &Handler{catalog: catalog} }
func principal(c *gin.Context, manage bool) (auth.Principal, bool) {
	p, ok := auth.FromContext(c.Request.Context())
	if !ok || p.TenantID == "" {
		c.JSON(401, gin.H{"code": "UNAUTHENTICATED", "message": "未登录"})
		return p, false
	}
	permission := "dictionary.view"
	if manage {
		permission = "dictionary.manage"
	}
	if !p.HasPermission(permission) {
		c.JSON(403, gin.H{"code": "FORBIDDEN", "message": "无指标字典操作权限"})
		return p, false
	}
	// 租户级字典写入（PUT /dictionary、POST/PUT/PATCH/DELETE /dictionary/metrics）影响全租户，
	// 只有 TENANT 数据范围可以改动；ORG/SELF 主体即使有管理权限也只读。
	// 口径与 standardfacts 的全租户管理门禁一致（ScopeMode == "TENANT"）。
	if manage && !p.HasTenantScope() {
		c.JSON(403, gin.H{"code": "TENANT_SCOPE_REQUIRED", "message": "指标字典配置仅允许具有全租户数据范围的管理员操作"})
		return p, false
	}
	return p, true
}

// respondError 用途：输出指标治理的公开原因和安全字段问题。
// 参数：c 为HTTP上下文，e 为治理错误或不可公开的底层异常。
// 返回值：无；仅已知业务原因可进入错误回执。
func respondError(c *gin.Context, e error) {
	code := "METRIC_STORAGE_FAILED"
	status := 500
	message := "指标字典存储失败"
	var typed *application.Error
	if errors.As(e, &typed) {
		code = typed.Code
		status = 400
		message = "指标请求无效"
		switch code {
		case "METRIC_NOT_FOUND":
			status = 404
			message = "指标不存在"
		case "METRIC_VERSION_CONFLICT":
			status = 409
			message = "指标已更新，请刷新后重试"
		case "METRIC_CODE_CONFLICT":
			status = 409
			message = "指标编码已存在或已保留"
		case "METRIC_BUILTIN_PROTECTED":
			status = 409
			message = "内置指标不能删除"
		case "METRIC_REFERENCED":
			status = 409
			message = "指标已有引用，不能删除"
		case "CALCULATION_BINDING_REQUIRED":
			message = "指标未绑定受控计算执行器，不能启用"
		case "METRIC_VALIDATION_FAILED":
			message = "指标内容未通过校验，请核对提示字段、长度和编码格式"
		case "METRIC_LIMIT_EXCEEDED":
			message = "当前租户已达到500条指标定义记录上限，本次未创建；已删除记录仍保留，请联系管理员核对指标容量"
		case "METRIC_STORAGE_FAILED":
			status = 500
			message = "指标字典存储失败"
		}
	}
	body := gin.H{"code": code, "message": message}
	if typed != nil && len(typed.Issues) > 0 {
		body["details"] = gin.H{"issues": typed.Issues}
	}
	c.JSON(status, body)
}
func invalid(c *gin.Context) {
	c.JSON(400, gin.H{"code": "METRIC_PAYLOAD_INVALID", "message": "指标定义格式无效"})
}
func (h *Handler) Get(c *gin.Context) {
	p, ok := principal(c, false)
	if !ok {
		return
	}
	v, e := h.catalog.ListForTenant(c.Request.Context(), p.TenantID)
	if e != nil {
		respondError(c, e)
		return
	}
	response.OK(c, v)
}
func (h *Handler) Put(c *gin.Context) {
	p, ok := principal(c, true)
	if !ok {
		return
	}
	var v []domain.Metric
	if c.ShouldBindJSON(&v) != nil {
		invalid(c)
		return
	}
	if e := h.catalog.SaveTenantMetrics(c.Request.Context(), p.TenantID, p.UserID, v); e != nil {
		respondError(c, e)
		return
	}
	catalog, e := h.catalog.ListForTenant(c.Request.Context(), p.TenantID)
	if e != nil {
		respondError(c, e)
		return
	}
	response.OK(c, catalog)
}
func (h *Handler) Create(c *gin.Context) {
	p, ok := principal(c, true)
	if !ok {
		return
	}
	var v domain.Metric
	if c.ShouldBindJSON(&v) != nil {
		invalid(c)
		return
	}
	out, e := h.catalog.Create(c.Request.Context(), p.TenantID, p.UserID, v)
	if e != nil {
		respondError(c, e)
		return
	}
	response.OK(c, out)
}
func (h *Handler) Update(c *gin.Context) {
	p, ok := principal(c, true)
	if !ok {
		return
	}
	var v domain.Metric
	if c.ShouldBindJSON(&v) != nil {
		invalid(c)
		return
	}
	if v.Version == 0 {
		respondError(c, &application.Error{Code: "METRIC_VERSION_CONFLICT"})
		return
	}
	out, e := h.catalog.Update(c.Request.Context(), p.TenantID, p.UserID, c.Param("code"), v)
	if e != nil {
		respondError(c, e)
		return
	}
	response.OK(c, out)
}
func (h *Handler) SetEnabled(c *gin.Context) {
	p, ok := principal(c, true)
	if !ok {
		return
	}
	var v struct {
		Enabled *bool  `json:"enabled"`
		Version uint64 `json:"version"`
	}
	if c.ShouldBindJSON(&v) != nil || v.Enabled == nil {
		invalid(c)
		return
	}
	if v.Version == 0 {
		respondError(c, &application.Error{Code: "METRIC_VERSION_CONFLICT"})
		return
	}
	out, e := h.catalog.SetEnabled(c.Request.Context(), p.TenantID, p.UserID, c.Param("code"), *v.Enabled, v.Version)
	if e != nil {
		respondError(c, e)
		return
	}
	response.OK(c, out)
}
func (h *Handler) Delete(c *gin.Context) {
	p, ok := principal(c, true)
	if !ok {
		return
	}
	var v struct {
		Version uint64 `json:"version"`
	}
	if c.ShouldBindJSON(&v) != nil {
		invalid(c)
		return
	}
	if v.Version == 0 {
		respondError(c, &application.Error{Code: "METRIC_VERSION_CONFLICT"})
		return
	}
	if e := h.catalog.Delete(c.Request.Context(), p.TenantID, p.UserID, c.Param("code"), v.Version); e != nil {
		respondError(c, e)
		return
	}
	response.OK(c, gin.H{"deleted": true})
}
func (h *Handler) History(c *gin.Context) {
	p, ok := principal(c, false)
	if !ok {
		return
	}
	out, e := h.catalog.History(c.Request.Context(), p.TenantID, c.Param("code"))
	if e != nil {
		respondError(c, e)
		return
	}
	response.OK(c, out)
}
