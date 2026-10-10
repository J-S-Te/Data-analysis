package commercial

import (
	"context"
	"errors"
	"github.com/gin-gonic/gin"
	"net/http/httptest"
	"testing"
)

func TestExpiryRetainsHistoryButRejectsBusinessAndEmbedding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	check := Check(func(_ context.Context, operation string) error {
		if operation == MutateBusiness {
			return errors.New("expired")
		}
		return nil
	})
	r := gin.New()
	r.Use(Middleware(check))
	for _, p := range []string{"/api/v1/dashboard/project", "/api/v1/embed/:dashboard", "/api/v1/embed-proxy/:token", "/api/v1/embed-proxy/:token/*resource"} {
		r.GET(p, func(c *gin.Context) { c.Status(200) })
	}
	r.POST("/api/v1/admin/sources/trigger-all", func(c *gin.Context) { c.Status(200) })
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/api/v1/dashboard/project", 200}, {"GET", "/api/v1/embed/contract", 403},
		{"GET", "/api/v1/embed-proxy/old-token", 403}, {"GET", "/api/v1/embed-proxy/old-token/api/embed/dashboard/query", 403},
		{"POST", "/api/v1/admin/sources/trigger-all", 403},
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.want {
			t.Fatalf("%s got %d", tc.path, w.Code)
		}
	}
}
