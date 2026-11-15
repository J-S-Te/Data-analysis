package http

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/dictionary/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// TestMetricFeedbackIdentifiesInvalidField 用途：验证真实指标请求的校验结果包含安全字段坐标。
// 参数：t 为只使用内存 SQLite 的 HTTP 测试上下文。
// 返回值：无；字段、规则或现有错误码缺失时失败。
func TestMetricFeedbackIdentifiesInvalidField(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	h := NewHandler(application.NewCatalogService(db))
	principal := auth.Principal{TenantID: "tenant", UserID: "actor", ScopeMode: "TENANT",
		Permissions: map[string]struct{}{"dictionary.manage": {}}}
	w := invoke(t, h.Create, &principal, `{"code":"custom.kpi","name":" ","dashboard":"看板","definition":"定义","formula":"公式","source":"来源","period":"月","status":"待确认"}`)
	var body struct {
		Code    string
		Details struct {
			Issues []struct{ Field, Rule, Message string }
		}
	}
	if err = json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != 400 || body.Code != "METRIC_VALIDATION_FAILED" || len(body.Details.Issues) != 1 {
		t.Fatalf("字段回执缺失: %d %s", w.Code, w.Body)
	}
	issue := body.Details.Issues[0]
	if issue.Field != "name" || issue.Rule != "required" || !strings.Contains(issue.Message, "名称") {
		t.Fatalf("字段语义错误: %+v", issue)
	}
}

// TestMetricFeedbackExplainsReservedCapacity 用途：验证容量拒绝解释原有上限且不承诺删除释放名额。
// 参数：t 为纯 HTTP 错误回执测试上下文。
// 返回值：无；缺少容量和保留记录提示或错误分类变化时失败。
func TestMetricFeedbackExplainsReservedCapacity(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	respondError(c, &application.Error{Code: "METRIC_LIMIT_EXCEEDED"})
	if w.Code != 400 || !strings.Contains(w.Body.String(), "500") || !strings.Contains(w.Body.String(), "保留") {
		t.Fatalf("容量提示缺失: %d %s", w.Code, w.Body)
	}
}
