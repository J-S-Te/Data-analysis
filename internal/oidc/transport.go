package oidc

import (
	"errors"
	"net/http"
	"net/url"
)

// hostRewriteTransport 把容器内请求的主机重写为 backchannel 地址（Keycloak 仅内网可达，
// 而 discovery 返回的 issuer 是公网地址，go-oidc 要求 issuer 严格匹配，故保留公网 issuer、
// 只重写实际连接目标）。对齐 contract 的 OIDC_BACKCHANNEL_BASE_URL 模式。
type hostRewriteTransport struct {
	base         http.RoundTripper
	targetHost   string
	targetScheme string
}

func newHostRewriteTransport(backchannel string) (http.RoundTripper, error) {
	parsed, err := url.Parse(backchannel)
	if err != nil || parsed.Host == "" {
		return nil, errors.New("invalid backchannel issuer")
	}
	return &hostRewriteTransport{base: http.DefaultTransport, targetHost: parsed.Host, targetScheme: parsed.Scheme}, nil
}

func (t *hostRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.targetHost != "" {
		req.URL.Host = t.targetHost
		if t.targetScheme != "" {
			req.URL.Scheme = t.targetScheme
		}
	}
	return t.base.RoundTrip(req)
}
