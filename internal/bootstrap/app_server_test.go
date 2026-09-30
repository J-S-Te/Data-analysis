package bootstrap

import (
	"net/http"
	"testing"
	"time"
)

// AUD-2026-031：dashboard-api 的 http.Server 必须配置四项超时（slowloris 面），
// 数值与 project_management/cmd/api/main.go 的既有口径一致。
func TestNewHTTPServerSetsConnectionTimeouts(t *testing.T) {
	server := newHTTPServer("127.0.0.1:0", http.NewServeMux())
	if server == nil {
		t.Fatal("newHTTPServer() = nil")
	}
	if server.Addr != "127.0.0.1:0" || server.Handler == nil {
		t.Fatalf("addr/handler not wired: addr = %q", server.Addr)
	}
	want := map[string]time.Duration{
		"ReadHeaderTimeout": 5 * time.Second,
		"ReadTimeout":       15 * time.Second,
		"WriteTimeout":      45 * time.Second,
		"IdleTimeout":       60 * time.Second,
	}
	got := map[string]time.Duration{
		"ReadHeaderTimeout": server.ReadHeaderTimeout,
		"ReadTimeout":       server.ReadTimeout,
		"WriteTimeout":      server.WriteTimeout,
		"IdleTimeout":       server.IdleTimeout,
	}
	for name, expected := range want {
		if got[name] != expected {
			t.Fatalf("%s = %v, want %v", name, got[name], expected)
		}
	}
}
