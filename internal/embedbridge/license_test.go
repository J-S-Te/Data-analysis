package embedbridge

import (
	"context"
	"errors"
	"github.com/gin-gonic/gin"
	"net/http/httptest"
	"testing"
)

func TestLicenseDeniesAllEmbedPathsBeforeDatabaseOrUpstream(t *testing.T) {
	b, err := New(nil, Options{MetabaseInternalURL: "http://127.0.0.1:1", EmbeddingSecret: "isolated-test-secret", LicenseCheck: func(context.Context, string) error { return errors.New("expired") }})
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range []func(*gin.Context){func(c *gin.Context) { b.Issue(c, "contract") }, func(c *gin.Context) { b.Proxy(c, "old-token") }, func(c *gin.Context) { b.ProxyResource(c, "old-token", "/app.js") }} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/", nil)
		run(c)
		if w.Code != 403 {
			t.Fatal(w.Code)
		}
	}
}
