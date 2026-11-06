package bootstrap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/unified-identity-auth-platform/data-analysis/internal/auditoutbox"
	"github.com/unified-identity-auth-platform/data-analysis/internal/metrics"
)

// stubOutbox 固定返回三态计数，避免路由测试依赖数据库。
type stubOutbox struct{}

func (stubOutbox) Counts(context.Context) (auditoutbox.OutboxCounts, error) {
	return auditoutbox.OutboxCounts{Pending: 3, Delivered: 11, Dead: 1}, nil
}

func metricsScrape(t *testing.T, router *gin.Engine, path, remoteAddr, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.RemoteAddr = remoteAddr
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// C2：/metrics 挂在服务根路径（抓取地址是部署契约），不是 APP_PATH_PREFIX 下的第二个副本；
// 未配置 token 时非回环抓取 403，回环 200。
func TestMountMetricsServesRootPathWithLoopbackOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	mountMetrics(router, "", &metrics.Handler{Outbox: stubOutbox{}})

	loopback := metricsScrape(t, router, "/metrics", "127.0.0.1:40000", "")
	if loopback.Code != http.StatusOK {
		t.Fatalf("loopback /metrics status = %d, want 200", loopback.Code)
	}
	body := loopback.Body.String()
	for _, want := range []string{
		"data_analysis_audit_outbox_pending 3\n",
		"data_analysis_audit_outbox_delivered 11\n",
		"data_analysis_audit_outbox_dead 1\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("exposition missing %q:\n%s", want, body)
		}
	}
	if remote := metricsScrape(t, router, "/metrics", "10.9.8.7:40000", ""); remote.Code != http.StatusForbidden {
		t.Fatalf("remote /metrics status = %d, want 403", remote.Code)
	}
	prefixed := metricsScrape(t, router, "/data_analysis/metrics", "127.0.0.1:40000", "")
	if prefixed.Code != http.StatusNotFound {
		t.Fatalf("prefixed /data_analysis/metrics status = %d, want 404 (single authoritative mount)", prefixed.Code)
	}
}

// 配置 METRICS_ACCESS_TOKEN 后远端抓取凭 Bearer 放行，缺失令牌仍拒绝。
func TestMountMetricsRequiresBearerWhenTokenConfigured(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	mountMetrics(router, "scrape-token", &metrics.Handler{Outbox: stubOutbox{}})

	if remote := metricsScrape(t, router, "/metrics", "10.9.8.7:40000", "Bearer scrape-token"); remote.Code != http.StatusOK {
		t.Fatalf("authorized remote /metrics status = %d, want 200", remote.Code)
	}
	if anonymous := metricsScrape(t, router, "/metrics", "10.9.8.7:40000", ""); anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous remote /metrics status = %d, want 401", anonymous.Code)
	}
}
