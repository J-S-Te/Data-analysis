package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// SEC-D11：cookie 写请求的 Origin 必须存在且与 PublicOrigin 一致；
// 缺失、跨源、缺 CSRF 头、PublicOrigin 为空（失败关闭）一律被拒。
func TestRequireSameOriginWriteRejectsMissingAndCrossOrigin(t *testing.T) {
	tests := []struct {
		name         string
		publicOrigin string
		origin       string
		csrf         string
		secFetchSite string
		wantStatus   int
	}{
		{name: "same origin passes", publicOrigin: "https://platform.example.com", origin: "https://platform.example.com", csrf: "1", wantStatus: http.StatusNoContent},
		{name: "missing origin", publicOrigin: "https://platform.example.com", csrf: "1", wantStatus: http.StatusForbidden},
		{name: "cross origin", publicOrigin: "https://platform.example.com", origin: "https://evil.example", csrf: "1", wantStatus: http.StatusForbidden},
		{name: "missing csrf header", publicOrigin: "https://platform.example.com", origin: "https://platform.example.com", wantStatus: http.StatusForbidden},
		{name: "empty public origin with missing origin", publicOrigin: "", csrf: "1", wantStatus: http.StatusForbidden},
		{name: "empty public origin with matching origin", publicOrigin: "", origin: "https://platform.example.com", csrf: "1", wantStatus: http.StatusForbidden},
		{name: "malformed origin", publicOrigin: "https://platform.example.com", origin: "null", csrf: "1", wantStatus: http.StatusForbidden},
		{name: "cross-site fetch metadata", publicOrigin: "https://platform.example.com", origin: "https://platform.example.com", csrf: "1", secFetchSite: "cross-site", wantStatus: http.StatusForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.Use(RequireSameOriginWrite(test.publicOrigin))
			router.POST("/write", func(c *gin.Context) { c.Status(http.StatusNoContent) })
			router.GET("/read", func(c *gin.Context) { c.Status(http.StatusOK) })

			request := httptest.NewRequest(http.MethodPost, "/write", nil)
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if test.csrf != "" {
				request.Header.Set("X-CSRF-Token", test.csrf)
			}
			if test.secFetchSite != "" {
				request.Header.Set("Sec-Fetch-Site", test.secFetchSite)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
}

// SEC-D11：读请求不受同源校验影响，即使 PublicOrigin 为空。
func TestRequireSameOriginWriteLeavesReadsUntouchedWhenPublicOriginEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequireSameOriginWrite(""))
	router.GET("/read", func(c *gin.Context) { c.Status(http.StatusOK) })

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/read", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
}
