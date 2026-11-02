package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func embedProxyCORSRouter(embedPublicOrigin string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(EmbedProxyCORS(embedPublicOrigin))
	router.GET("/api/v1/embed-proxy/:token/app/dist/x.js", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	return router
}

func embedProxyCORSRequest(t *testing.T, router *gin.Engine, method, origin string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/v1/embed-proxy/tok/app/dist/x.js", nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if method == http.MethodOptions {
		req.Header.Set("Access-Control-Request-Method", http.MethodGet)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// 不透明源（Origin: null）绝不能被回显为 ACAO：令牌可能出现在截图/日志/共享链接里，
// 任意不透明源页面都能借 sandbox iframe 跨源读取代理内容（S-4）。
func TestEmbedProxyCORSDoesNotEchoOpaqueOrigin(t *testing.T) {
	for _, configured := range []string{"https://embed.example.net", ""} {
		router := embedProxyCORSRouter(configured)

		rec := embedProxyCORSRequest(t, router, http.MethodGet, "null")
		if rec.Code != http.StatusOK {
			t.Fatalf("configured=%q status = %d, want 200", configured, rec.Code)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Fatalf("configured=%q Access-Control-Allow-Origin = %q, want empty for Origin: null", configured, got)
		}

		// 预检同样不回显 ACAO，浏览器据此失败关闭。
		rec = embedProxyCORSRequest(t, router, http.MethodOptions, "null")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("configured=%q preflight status = %d, want 204", configured, rec.Code)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Fatalf("configured=%q preflight Access-Control-Allow-Origin = %q, want empty", configured, got)
		}
		if got := rec.Header().Get("Access-Control-Allow-Methods"); got == "" {
			t.Fatalf("configured=%q Access-Control-Allow-Methods missing", configured)
		}
	}
}

// 配置的独立嵌入源精确命中时才回显该 origin。
func TestEmbedProxyCORSEchoesConfiguredOrigin(t *testing.T) {
	router := embedProxyCORSRouter("https://embed.example.net")

	rec := embedProxyCORSRequest(t, router, http.MethodGet, "https://embed.example.net")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://embed.example.net" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want https://embed.example.net", got)
	}
	if got := rec.Header().Get("Vary"); got != "Origin" {
		t.Fatalf("Vary = %q, want Origin", got)
	}

	rec = embedProxyCORSRequest(t, router, http.MethodOptions, "https://embed.example.net")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://embed.example.net" {
		t.Fatalf("preflight Access-Control-Allow-Origin = %q, want https://embed.example.net", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Fatalf("preflight Access-Control-Allow-Methods missing")
	}
}

// 其他来源（含子域、后缀伪装、协议/端口不同、缺 Origin）一律不回显 ACAO，不扩大允许来源。
func TestEmbedProxyCORSRejectsOtherOrigins(t *testing.T) {
	router := embedProxyCORSRouter("https://embed.example.net")

	for _, origin := range []string{
		"https://evil.example.com",
		"https://evil.embed.example.net",
		"https://embed.example.net.evil.com",
		"http://embed.example.net",
		"https://embed.example.net:8443",
		"",
	} {
		rec := embedProxyCORSRequest(t, router, http.MethodGet, origin)
		if rec.Code != http.StatusOK {
			t.Fatalf("origin=%q status = %d, want 200", origin, rec.Code)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Fatalf("origin=%q Access-Control-Allow-Origin = %q, want empty", origin, got)
		}
	}
}

// 未配置或配置非法（带路径、缺 scheme、字面量 null 等）时失败关闭：任何来源都不回显 ACAO。
func TestEmbedProxyCORSFailsClosedWithoutUsableConfiguration(t *testing.T) {
	for _, configured := range []string{"", "   ", "null", "https://embed.example.net/path", "embed.example.net"} {
		router := embedProxyCORSRouter(configured)
		for _, origin := range []string{"https://embed.example.net", "null"} {
			rec := embedProxyCORSRequest(t, router, http.MethodGet, origin)
			if rec.Code != http.StatusOK {
				t.Fatalf("configured=%q origin=%q status = %d, want 200", configured, origin, rec.Code)
			}
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
				t.Fatalf("configured=%q origin=%q Access-Control-Allow-Origin = %q, want empty", configured, origin, got)
			}
		}
	}
}
