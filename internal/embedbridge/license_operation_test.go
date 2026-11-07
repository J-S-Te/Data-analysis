package embedbridge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/unified-identity-auth-platform/data-analysis/internal/commercial"
)

// TestLicenseDenialSettlesMutateBusiness 验证启用模式的三条嵌入路径在许可拒绝时
// 都以 MutateBusiness 结算，而不只是"返回了 403"。
// 理由：嵌入授权签发与代理都是 GET，如果被归入历史读取，只有读授权的许可证
// 也能签发嵌入能力；只断言状态码无法发现这种分类漂移。
// 代理路径必须先通过隔离嵌入来源校验，否则请求在到达许可门禁之前就被拒绝，
// 断言会变成"来源校验返回 403"的空转，因此这里显式绑定独立嵌入来源。
func TestLicenseDenialSettlesMutateBusiness(t *testing.T) {
	var recorded []string
	check := commercial.Check(func(ctx context.Context, operation string) error {
		recorded = append(recorded, operation)
		return errors.New("expired")
	})
	bridge, err := New(nil, Options{
		MetabaseInternalURL: "http://127.0.0.1:1", EmbeddingSecret: "isolated-test-secret",
		PublicOrigin: "https://app.example.com", EmbedPublicOrigin: "https://embed.example.net",
		LicenseCheck: check,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range []func(*gin.Context){
		func(c *gin.Context) { bridge.Issue(c, "contract") },
		func(c *gin.Context) { bridge.Proxy(c, "old-token") },
		func(c *gin.Context) { bridge.ProxyResource(c, "old-token", "/app.js") },
	} {
		recorded = nil
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		// 代理路径与嵌入文档同源校验：Host 必须命中独立嵌入来源。
		c.Request.Host = "embed.example.net"
		run(c)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status=%d body=%s want 403", w.Code, w.Body.String())
		}
		if len(recorded) != 1 || recorded[0] != commercial.MutateBusiness {
			t.Fatalf("license operations=%v want exactly [%s]", recorded, commercial.MutateBusiness)
		}
	}
}
