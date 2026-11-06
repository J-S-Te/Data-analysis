package standardfacts

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/apperror"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/response"
)

// Handler 在既有认证、许可和同源保护组内暴露标准事实管理。
type Handler struct {
	Store         *Store
	FinanceSource string
}

// Register 挂载当前财务来源的模板、状态、预览与原子发布接口。
// 参数：api 为既有会话、同源及商业许可保护组。
// 返回值：无；处理器继续强制管理权限和显式全租户范围。
func (h *Handler) Register(api *gin.RouterGroup) {
	group := api.Group("/admin/standard-finance", h.authorize)
	group.GET("/template", h.Template)
	group.GET("/status", h.Status)
	group.POST("/preview", h.Preview)
	group.POST("/publish", h.Publish)
}

// authorize 校验可信主体、聚合管理权限和全租户数据范围。
// 参数：c 为已通过外层会话和许可门禁的上下文。
// 返回值：无；缺少任一边界时停止请求。
func (h *Handler) authorize(c *gin.Context) {
	principal, ok := auth.FromContext(c.Request.Context())
	if !ok || principal.TenantID == "" || principal.UserID == "" {
		response.Error(c, apperror.ErrUnauthenticated)
		c.Abort()
		return
	}
	if !principal.HasPermission("aggregation.manage") {
		response.Error(c, apperror.ErrForbidden)
		c.Abort()
		return
	}
	if principal.ScopeMode != "TENANT" {
		response.Error(c, apperror.New(403, "STANDARD_FACTS_SCOPE_DENIED", "标准财务事实仅允许具有全租户数据范围的管理员管理"))
		c.Abort()
		return
	}
	c.Next()
}

// Template 返回可下载的无模拟事实模板和字段业务说明。
// 参数：c 为已授权管理请求上下文。
// 返回值：无；空批次号要求使用者明确填写，空数组代表已确认无数据。
func (h *Handler) Template(c *gin.Context) {
	date := time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01-02")
	response.OK(c, gin.H{"filename": "standard-finance-v1.json", "document": Document{SchemaVersion: 1, AsOfDate: date, Complete: true, Receivables: []Receivable{}, Receipts: []Receipt{}}, "max_rows": MaxRows, "max_bytes": MaxFileBytes,
		"guide": []string{"仅导入截至日完整已确认应收及有效回款；每次发布替换本来源上一版，不与联动来源相加。", "batch_id：1至64位字母数字及_.:-；同一编号只对应同一文件内容。", "as_of_date、due_date、receipt_date：YYYY-MM-DD；截至日不能晚于北京时间今天，回款日不能晚于截至日。", "external_id：各类事实内唯一的外部编号；currency：三位大写币种代码。", "original_minor、allocated_minor、amount_minor：整数，单位为币种金额的1/100，单条最高100亿元；核销额不得超过原应收，回款须大于零。", "receivables：已确认应收，填写原应收、已核销额、到期日；receipts：已有效登记回款，填写金额、登记日。", "complete=true：确认两类事实均完整；合计最多1000条、1MiB；空数组将发布真实零值，须在预览中核对。", "未支持收入确认、部门成本、未确认应收、作废回款和退款；请勿将它们混入文件。"}})
}

// Status 返回选中来源和当前租户发布版本，不把存储故障解释为未发布。
// 参数：c 为已授权管理请求上下文。
// 返回值：无；联动模式不查询标准事实表。
func (h *Handler) Status(c *gin.Context) {
	state := Publication{SourceCode: SourceCode}
	if h.FinanceSource == "standard" {
		principal, _ := auth.FromContext(c.Request.Context())
		var err error
		state, err = h.Store.Status(c.Request.Context(), principal.TenantID)
		if err != nil {
			response.Error(c, apperror.New(503, "STANDARD_FACTS_UNAVAILABLE", "标准财务来源暂不可读，请稍后重试"))
			return
		}
	}
	response.OK(c, gin.H{"selected_source": h.FinanceSource, "publication": state})
}

// requireStandard 拒绝向未选中的标准来源预览或发布，避免产生不可见的业务修改。
// 参数：c 为当前管理请求。
// 返回值：是否选中标准来源；否则返回明确未启用错误。
func (h *Handler) requireStandard(c *gin.Context) bool {
	if h.FinanceSource == "standard" {
		return true
	}
	response.Error(c, apperror.New(503, "STANDARD_FACTS_SOURCE_DISABLED", "当前财务看板使用联动来源，标准事实导入尚未启用"))
	return false
}

// Preview 严格校验文件并返回可核对的真实财务总额与当前版本。
// 参数：c 为上传标准 JSON 文件的管理请求。
// 返回值：无；预览不写事实及快照，文件或依赖失败时不返回部分成功。
func (h *Handler) Preview(c *gin.Context) {
	if !h.requireStandard(c) {
		return
	}
	if c.ContentType() != "application/json" {
		response.Error(c, apperror.New(400, "STANDARD_FACTS_INVALID", "请上传标准 JSON 文件"))
		return
	}
	doc, err := DecodeDocument(c.Request.Body)
	if err != nil {
		response.Error(c, apperror.New(400, "STANDARD_FACTS_INVALID", err.Error()))
		return
	}
	// 先区分文件非法与存储不可读，避免泄漏数据库内部错误。
	if _, err := Analyze(doc, time.Now()); err != nil {
		response.Error(c, apperror.New(400, "STANDARD_FACTS_INVALID", err.Error()))
		return
	}
	principal, _ := auth.FromContext(c.Request.Context())
	preview, err := h.Store.Preview(c.Request.Context(), principal.TenantID, doc, time.Now())
	if err != nil {
		response.Error(c, apperror.New(503, "STANDARD_FACTS_UNAVAILABLE", "标准财务来源暂不可读，请稍后重试"))
		return
	}
	response.OK(c, preview)
}

// Publish 只在指纹、事实校验与当前来源版本均吻合时执行原子发布。
// 参数：c 为带完整文件、预览指纹和预期版本的同源管理请求。
// 返回值：无；冲突返回409，失败不发布部分事实。
func (h *Handler) Publish(c *gin.Context) {
	if !h.requireStandard(c) {
		return
	}
	var request PublishRequest
	if err := decodePublish(c, &request); err != nil {
		response.Error(c, apperror.New(400, "STANDARD_FACTS_INVALID", err.Error()))
		return
	}
	if _, err := Analyze(request.Document, time.Now()); err != nil {
		response.Error(c, apperror.New(400, "STANDARD_FACTS_INVALID", err.Error()))
		return
	}
	principal, _ := auth.FromContext(c.Request.Context())
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	result, err := h.Store.Publish(ctx, principal.TenantID, principal.UserID, c.GetString("request_id"), request, time.Now())
	switch {
	case err == nil:
		response.OK(c, result)
	case errors.Is(err, ErrLicenseDenied):
		response.Error(c, apperror.New(403, "COMMERCIAL_LICENSE_DENIED", err.Error()))
	case errors.Is(err, ErrVersionConflict):
		response.Error(c, apperror.New(409, "STANDARD_FACTS_VERSION_CONFLICT", err.Error()))
	case errors.Is(err, ErrBatchConflict):
		response.Error(c, apperror.New(409, "STANDARD_FACTS_BATCH_CONFLICT", err.Error()))
	case errors.Is(err, ErrInvalidPreview):
		response.Error(c, apperror.New(400, "STANDARD_FACTS_PREVIEW_MISMATCH", err.Error()))
	default:
		response.Error(c, apperror.New(503, "STANDARD_FACTS_PUBLISH_FAILED", "发布未完成，请先刷新来源状态核对批次，勿更换批次编号重复发布"))
	}
}

// decodePublish 有界严格读取发布请求，不接受客户端租户或多余内容。
// 参数：c 为 JSON 请求，request 为解析目标。
// 返回值：格式或大小错误，不返回内部解析细节。
func decodePublish(c *gin.Context, request *PublishRequest) error {
	if c.ContentType() != "application/json" {
		return errors.New("发布请求须为标准 JSON")
	}
	reader := http.MaxBytesReader(c.Writer, c.Request.Body, MaxFileBytes+4096)
	raw, err := io.ReadAll(reader)
	if err != nil {
		return errors.New("发布文件格式不正确或超过大小限制")
	}
	fields := []string{"document", "digest", "expected_version"}
	if err := decodeRequiredFact(raw, request, fields); err != nil {
		return errors.New("发布请求须提供完整文件、预览指纹及非负整数版本，且不能包含额外字段")
	}
	if request.ExpectedVersion < 0 {
		return errors.New("发布版本必须为非负整数")
	}
	return nil
}
