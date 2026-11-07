package metrics

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/unified-identity-auth-platform/data-analysis/internal/auditoutbox"
)

// fakeOutbox 是可控的计数源；测试不依赖真实数据库。
type fakeOutbox struct {
	counts auditoutbox.OutboxCounts
	err    error
	calls  int
}

func (f *fakeOutbox) Counts(context.Context) (auditoutbox.OutboxCounts, error) {
	f.calls++
	return f.counts, f.err
}

func newRouter(outbox OutboxCounter, accessToken string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := &Handler{Outbox: outbox, StartedAt: time.Unix(1_700_000_000, 0)}
	router.GET(Path, RequireAccess(accessToken), handler.Handle)
	return router
}

func scrape(t *testing.T, router *gin.Engine, remoteAddr, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, Path, nil)
	request.RemoteAddr = remoteAddr
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// SEC-B4 / C2：未配置 token 时只有回环可达；非回环抓取必须 403（fail-closed）。
func TestMetricsWithoutTokenOnlyLoopback(t *testing.T) {
	router := newRouter(&fakeOutbox{}, "")
	for _, tc := range []struct {
		name       string
		remoteAddr string
		want       int
	}{
		{name: "ipv4 loopback", remoteAddr: "127.0.0.1:54321", want: http.StatusOK},
		{name: "ipv6 loopback", remoteAddr: "[::1]:54321", want: http.StatusOK},
		{name: "private network peer", remoteAddr: "10.1.2.3:54321", want: http.StatusForbidden},
		{name: "public peer", remoteAddr: "203.0.113.9:54321", want: http.StatusForbidden},
		{name: "unparsable peer", remoteAddr: "not-an-address", want: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := scrape(t, router, tc.remoteAddr, "")
			if recorder.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %q)", recorder.Code, tc.want, recorder.Body.String())
			}
			if tc.want == http.StatusOK {
				if body := recorder.Body.String(); !strings.Contains(body, "data_analysis_audit_outbox_pending") {
					t.Fatalf("loopback scrape body missing metrics: %q", body)
				}
			}
		})
	}
}

// 伪造代理头不能把远端抓取说成回环：只看 TCP 对端。
func TestMetricsIgnoresForwardedForSpoof(t *testing.T) {
	router := newRouter(&fakeOutbox{}, "")
	request := httptest.NewRequest(http.MethodGet, Path, nil)
	request.RemoteAddr = "203.0.113.9:54321"
	request.Header.Set("X-Forwarded-For", "127.0.0.1")
	request.Header.Set("X-Real-IP", "127.0.0.1")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", recorder.Code)
	}
}

// 配置 token 后：正确 Bearer 放行（任何对端），缺失/错误 token 一律 401。
func TestMetricsWithTokenRequiresBearer(t *testing.T) {
	router := newRouter(&fakeOutbox{}, "scrape-token")
	for _, tc := range []struct {
		name          string
		remoteAddr    string
		authorization string
		want          int
	}{
		{name: "valid token from remote peer", remoteAddr: "10.1.2.3:54321", authorization: "Bearer scrape-token", want: http.StatusOK},
		{name: "valid token from loopback", remoteAddr: "127.0.0.1:54321", authorization: "Bearer scrape-token", want: http.StatusOK},
		{name: "missing token", remoteAddr: "10.1.2.3:54321", want: http.StatusUnauthorized},
		{name: "missing token on loopback", remoteAddr: "127.0.0.1:54321", want: http.StatusUnauthorized},
		{name: "wrong token", remoteAddr: "10.1.2.3:54321", authorization: "Bearer other", want: http.StatusUnauthorized},
		{name: "non bearer scheme", remoteAddr: "10.1.2.3:54321", authorization: "Basic scrape-token", want: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := scrape(t, router, tc.remoteAddr, tc.authorization)
			if recorder.Code != tc.want {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.want)
			}
		})
	}
}

// outbox 三态必须原样出现在 exposition 里，且失败时计数错误为 1。
func TestMetricsRendersOutboxThreeStates(t *testing.T) {
	router := newRouter(&fakeOutbox{counts: auditoutbox.OutboxCounts{Pending: 7, Delivered: 42, Dead: 2}}, "")
	body := scrape(t, router, "127.0.0.1:54321", "").Body.String()
	for _, want := range []string{
		"data_analysis_audit_outbox_pending 7\n",
		"data_analysis_audit_outbox_delivered 42\n",
		"data_analysis_audit_outbox_dead 2\n",
		"data_analysis_audit_outbox_counts_error 0\n",
		"data_analysis_process_start_time_seconds 1700000000\n",
		"data_analysis_build_info{",
		"data_analysis_process_goroutines ",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("exposition missing %q:\n%s", want, body)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if !strings.HasPrefix(line, "#") && !strings.Contains(line, " ") {
			t.Fatalf("malformed exposition line %q", line)
		}
	}
}

// 读取失败时不能伪装成 0 积压：样本值依旧可抓，但计数错误必须为 1。
func TestMetricsReportsOutboxReadFailure(t *testing.T) {
	outbox := &fakeOutbox{err: errors.New("table missing")}
	router := newRouter(outbox, "")
	recorder := scrape(t, router, "127.0.0.1:54321", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 so scrape stays up", recorder.Code)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "data_analysis_audit_outbox_counts_error 1\n") {
		t.Fatalf("failure not surfaced:\n%s", body)
	}
}

// store 未注入按配置缺失处理：仍可抓取，但计数错误为 1。
func TestMetricsWithoutOutboxCounter(t *testing.T) {
	router := newRouter(nil, "")
	body := scrape(t, router, "127.0.0.1:54321", "").Body.String()
	if !strings.Contains(body, "data_analysis_audit_outbox_counts_error 1\n") {
		t.Fatalf("missing counter not surfaced:\n%s", body)
	}
}
