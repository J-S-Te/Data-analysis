package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// 只有显式列入可信代理的对端才允许改写来源地址；私有网段不能凭地址段被信任。
func TestResolverTrustsExplicitProxyOnly(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "172.18.0.12:8080"
	req.Header.Set("X-Forwarded-For", "198.51.100.99, 125.120.19.87")

	if ip, source := NewClientIPResolver(nil).Resolve(req); ip != "172.18.0.12" || source != IPSourceDirect {
		t.Fatalf("未配置可信代理时 = (%q,%q)，want 直连对端", ip, source)
	}
	resolver := NewClientIPResolver([]string{"172.18.0.0/16"})
	if ip, source := resolver.Resolve(req); ip != "125.120.19.87" || source != IPSourceTrustedProxy {
		t.Fatalf("可信代理时 = (%q,%q)，want 最右非可信地址", ip, source)
	}
}

// AUD-IP-2026：办公网/VPN/回环都是合法操作地址，不能被丢弃。
func TestResolverKeepsPrivateAddresses(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "172.18.0.12:8080"
	req.Header.Set("X-Forwarded-For", "172.18.0.17")
	resolver := NewClientIPResolver([]string{"172.18.0.12/32"})
	if ip, source := resolver.Resolve(req); ip != "172.18.0.17" || source != IPSourceTrustedProxy {
		t.Fatalf("client IP=%q source=%q，want 私有地址保留", ip, source)
	}
}

// 非可信对端伪造代理头：地址必须回落到连接对端。
func TestResolverIgnoresSpoofedHeadersFromUntrustedPeer(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.5:12345"
	req.Header.Set("X-Real-IP", "8.8.8.8")
	req.Header.Set("X-Forwarded-For", "8.8.8.8, 1.2.3.4")
	if ip, source := NewClientIPResolver(nil).Resolve(req); ip != "203.0.113.5" || source != IPSourceDirect {
		t.Fatalf("client IP=%q source=%q，want 直连对端", ip, source)
	}
}

// 可信代理但没有可用转发头：仍然是直连观察。
func TestResolverFallsBackToPeerWithoutHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.10:5000"
	if ip, source := NewClientIPResolver([]string{"192.0.2.10/32"}).Resolve(req); ip != "192.0.2.10" || source != IPSourceDirect {
		t.Fatalf("client IP=%q source=%q", ip, source)
	}
}
