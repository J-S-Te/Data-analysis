package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequestClientIPUsesProxyPublicAddress(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "172.18.0.12:8080"
	req.Header.Set("X-Forwarded-For", "198.51.100.99, 125.120.19.87")
	if got := requestClientIP(req); got != "125.120.19.87" {
		t.Fatalf("client IP=%q, want right-most public address", got)
	}
}

func TestRequestClientIPRejectsPrivateAddresses(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "172.18.0.12:8080"
	req.Header.Set("X-Forwarded-For", "172.18.0.17")
	if got := requestClientIP(req); got != "" {
		t.Fatalf("client IP=%q, want empty", got)
	}
}
