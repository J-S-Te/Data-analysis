package middleware

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// 地址来源标记（与统一审计协议一致）。
const (
	IPSourceDirect       = "DIRECT"
	IPSourceTrustedProxy = "TRUSTED_PROXY"
	IPSourceUnknown      = "UNKNOWN"
)

// defaultTrustedProxies 只信任回环：任何非显式配置的对端都不能影响来源地址。
var defaultTrustedProxies = []string{"127.0.0.1/32", "::1/128"}

// ClientIPResolver 按显式可信代理列表解析客户端地址与来源。
//
// AUD-IP-2026 / 统一审计方案 §2.3：可信与否由**获取链路**决定，不能因为对端是私有网段
// 或回环就采信代理头；只有显式列入 trusted 的对端才允许改写来源地址。
type ClientIPResolver struct {
	prefixes []netip.Prefix
}

// NewClientIPResolver 解析 CIDR/IP 列表；空列表回退到只信任回环。
func NewClientIPResolver(values []string) ClientIPResolver {
	if len(values) == 0 {
		values = defaultTrustedProxies
	}
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		if prefix, err := netip.ParsePrefix(value); err == nil {
			prefixes = append(prefixes, prefix)
			continue
		}
		if address, err := netip.ParseAddr(value); err == nil {
			prefixes = append(prefixes, netip.PrefixFrom(address, address.BitLen()))
		}
	}
	return ClientIPResolver{prefixes: prefixes}
}

// Resolve 返回本次请求的客户端地址与来源。
func (resolver ClientIPResolver) Resolve(request *http.Request) (string, string) {
	if request == nil {
		return "", IPSourceUnknown
	}
	peer := peerAddress(request.RemoteAddr)
	if peer == "" {
		return "", IPSourceUnknown
	}
	if !resolver.trusts(peer) {
		return peer, IPSourceDirect
	}
	if ip := clientIPAddress(request.Header.Get("X-Real-IP")); ip != nil {
		if value := ip.String(); value != peer {
			return value, IPSourceTrustedProxy
		}
	}
	values := strings.Split(request.Header.Get("X-Forwarded-For"), ",")
	for index := len(values) - 1; index >= 0; index-- {
		ip := clientIPAddress(values[index])
		if ip == nil {
			continue
		}
		value := ip.String()
		if resolver.trusts(value) {
			// 从右向左跳过可信代理，取首个非可信地址。
			continue
		}
		if value != peer {
			return value, IPSourceTrustedProxy
		}
	}
	return peer, IPSourceDirect
}

func (resolver ClientIPResolver) trusts(value string) bool {
	address, err := netip.ParseAddr(value)
	if err != nil {
		return false
	}
	for _, prefix := range resolver.prefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func peerAddress(remoteAddr string) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(remoteAddr))
	if err != nil {
		host = strings.TrimSpace(remoteAddr)
	}
	address, err := netip.ParseAddr(host)
	if err != nil || address.IsUnspecified() || address.IsMulticast() {
		return ""
	}
	return address.String()
}
