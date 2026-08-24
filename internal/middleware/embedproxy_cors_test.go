package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestEmbedProxyCORSAllowsOnlyOpaqueOrigin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(EmbedProxyCORS())
	router.GET("/api/v1/embed-proxy/:token/app/dist/x.js", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	// opaque origin（sandbox 无 allow-same-origin）→ 允许跨源读取。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/embed-proxy/tok/app/dist/x.js", nil)
	req.Header.Set("Origin", "null")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "null" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want null", got)
	}

	// 明确的外源 origin → 不放宽。
	req = httptest.NewRequest(http.MethodGet, "/api/v1/embed-proxy/tok/app/dist/x.js", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want empty", got)
	}

	// 预检请求 → 204 + 允许方法。
	req = httptest.NewRequest(http.MethodOptions, "/api/v1/embed-proxy/tok/app/dist/x.js", nil)
	req.Header.Set("Origin", "null")
	req.Header.Set("Access-Control-Request-Method", "GET")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Fatalf("Access-Control-Allow-Methods missing")
	}
}
