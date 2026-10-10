package embedbridge

import (
	"errors"
	"net"
	"net/url"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// Embedded scripts must not share platform storage or a cookie domain. A
// different port alone does not isolate cookies, and sibling DNS names can
// overwrite parent-domain cookies. Require different registrable domains.
func isolatedEmbedOrigin(parent, embed string) (string, error) {
	parse := func(raw string) (*url.URL, error) {
		u, err := url.Parse(raw)
		if err != nil || u == nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return nil, errors.New("embed configuration requires plain origins")
		}
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "https" && !(u.Scheme == "http" && (strings.EqualFold(u.Hostname(), "localhost") || (ip != nil && ip.IsLoopback()))) {
			return nil, errors.New("embed origins require HTTPS except loopback testing")
		}
		return u, nil
	}
	p, err := parse(parent)
	if err != nil {
		return "", err
	}
	e, err := parse(embed)
	if err != nil {
		return "", err
	}
	if p.Scheme == "https" && e.Scheme != "https" {
		return "", errors.New("insecure embed origin")
	}
	pHost, eHost := strings.ToLower(p.Hostname()), strings.ToLower(e.Hostname())
	if pHost == eHost {
		return "", errors.New("embed origin shares platform cookie host")
	}
	pIP, eIP := net.ParseIP(pHost), net.ParseIP(eHost)
	if pIP == nil && eIP == nil {
		pDomain, pErr := publicsuffix.EffectiveTLDPlusOne(pHost)
		eDomain, eErr := publicsuffix.EffectiveTLDPlusOne(eHost)
		if pErr != nil || eErr != nil || pDomain == eDomain {
			return "", errors.New("embed origin shares platform cookie domain")
		}
	}
	return e.Scheme + "://" + e.Host, nil
}
