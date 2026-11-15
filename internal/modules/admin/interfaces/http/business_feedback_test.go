package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/unified-identity-auth-platform/data-analysis/internal/modules/admin/application"
	"github.com/unified-identity-auth-platform/data-analysis/internal/shared/auth"
)

type partialBatchFeedbackStub struct{ managementService }

// TriggerAllSources 用途：提供真实应用契约允许的前序已入队、后序失败结果。
// 参数：ctx 为请求上下文，tenant 为当前可信租户。
// 返回值：两个已入队、一个已跳过及后续失败。
func (*partialBatchFeedbackStub) TriggerAllSources(context.Context, string) (int, int, error) {
	return 2, 1, errors.New("internal queue write unavailable")
}

// TestBatchFeedbackRetainsConfirmedCounts 用途：验证 HTTP 失败回执仍保留真实前序完成数量。
// 参数：t 为不连接真实队列或数据库的 HTTP 测试上下文。
// 返回值：无；已入队数量丢失或底层异常进入公开提示时失败。
func TestBatchFeedbackRetainsConfirmedCounts(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest("POST", "/sources/sync-all", nil)
	c.Request = req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{
		TenantID: "tenant", UserID: "actor", ScopeMode: "TENANT", Permissions: map[string]struct{}{"aggregation.manage": {}},
	}))
	NewHandler(&partialBatchFeedbackStub{}).TriggerAllSources(c)
	var body struct {
		Code    string
		Details struct {
			Queued, Skipped   int
			PartialCompletion bool `json:"partial_completion"`
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != 500 || body.Code != "SYNC_ALL_FAILED" || body.Details.Queued != 2 || body.Details.Skipped != 1 || !body.Details.PartialCompletion {
		t.Fatalf("部分完成回执缺失: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "internal queue") {
		t.Fatalf("内部故障进入公开提示: %s", w.Body)
	}
}

// TestRuleFeedbackHTTPRequiresSavedSemantic 用途：防止通过中文错误文本猜测规则已提交。
// 参数：t 为规则 HTTP 适配器测试上下文。
// 返回值：无；明确已保存的标记丢失、错误伪判已保存或内部原因泄漏时失败。
func TestRuleFeedbackHTTPRequiresSavedSemantic(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		saved bool
	}{
		{"明确保存回执", errors.Join(application.ErrRuleSavedRefreshFailed, errors.New("private database read failed")), true},
		{"相同文本不是保存回执", errors.New("规则已保存，但最新列表读取失败"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			req := httptest.NewRequest("PUT", "/alert-rules", strings.NewReader(`[]`))
			req.Header.Set("Content-Type", "application/json")
			c.Request = req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{
				TenantID: "tenant", UserID: "actor", ScopeMode: "TENANT", Permissions: map[string]struct{}{"alert.manage": {}},
			}))
			NewHandler(&rulesHTTPStub{err: tc.err}).PutAlertRules(c)
			var body struct {
				Code    string
				Details struct {
					Saved         bool
					RefreshFailed bool `json:"refresh_failed"`
				}
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if w.Code != 500 || body.Details.Saved != tc.saved || body.Details.RefreshFailed != tc.saved {
				t.Fatalf("保存完成语义错误: %d %s", w.Code, w.Body)
			}
			if tc.saved && body.Code != "RULE_SAVED_REFRESH_FAILED" {
				t.Fatalf("已保存分类丢失: %s", w.Body)
			}
			if !tc.saved && body.Code != "RULE_SAVE_FAILED" {
				t.Fatalf("通过中文猜测保存成功: %s", w.Body)
			}
			if strings.Contains(w.Body.String(), "private") {
				t.Fatalf("内部故障进入公开回执: %s", w.Body)
			}
		})
	}
}
