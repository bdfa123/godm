package main

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// systemProxy is the manual proxy in the operating system's settings. On
// Windows that is where Clash, v2rayN and similar tools put theirs, and a
// program that only reads the proxy environment variables, as Go does by
// default, never sees it.
type systemProxy struct {
	byScheme map[string]*url.URL // "http=host:port" and "https=host:port"
	all      *url.URL            // a plain "host:port" serves every scheme
	socks    *url.URL            // "socks=host:port", for schemes without their own
	bypass   []string            // ProxyOverride patterns, lower case
	local    bool                // <local>: names without a dot go direct
}

// parseSystemProxy reads the three values Windows keeps: whether the proxy is
// on, the server list and the bypass list. The server list is either one
// "host:port" for everything or "http=h:p;https=h:p;socks=h:p". It returns nil
// when no proxy applies.
func parseSystemProxy(enabled bool, server, override string) *systemProxy {
	if !enabled {
		return nil
	}
	p := &systemProxy{byScheme: map[string]*url.URL{}}
	for _, entry := range strings.Split(server, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		scheme, addr, perScheme := strings.Cut(entry, "=")
		if !perScheme {
			if p.all == nil {
				p.all = proxyURL("http", entry)
			}
			continue
		}
		switch scheme = strings.ToLower(strings.TrimSpace(scheme)); scheme {
		case "http", "https":
			if u := proxyURL("http", addr); u != nil {
				p.byScheme[scheme] = u
			}
		case "socks":
			p.socks = proxyURL("socks5", addr)
		}
		// ftp= and the like name protocols godm never fetches.
	}
	if p.all == nil && p.socks == nil && len(p.byScheme) == 0 {
		return nil
	}
	for _, pat := range strings.Split(override, ";") {
		pat = strings.ToLower(strings.TrimSpace(pat))
		if i := strings.Index(pat, "://"); i >= 0 {
			pat = pat[i+3:]
		}
		switch pat {
		case "":
		case "<local>":
			p.local = true
		default:
			p.bypass = append(p.bypass, pat)
		}
	}
	return p
}

// proxyURL turns "host:port", or a full URL, into a proxy address. A bare
// address takes scheme: an http= or https= entry is an HTTP proxy either way,
// and socks= is SOCKS, which Go speaks as SOCKS5.
func proxyURL(scheme, addr string) *url.URL {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil
	}
	if !strings.Contains(addr, "://") {
		addr = scheme + "://" + addr
	}
	u, err := url.Parse(addr)
	if err != nil || u.Host == "" {
		return nil
	}
	switch u.Scheme = strings.ToLower(u.Scheme); u.Scheme {
	case "socks":
		u.Scheme = "socks5"
	case "http", "https", "socks5", "socks5h":
	default:
		return nil
	}
	return u
}

// proxyFor picks the proxy for one request, or nil to go direct. A scheme's
// own entry comes first, then the one for everything, then SOCKS, which is
// the order Windows applies them in.
func (p *systemProxy) proxyFor(u *url.URL) *url.URL {
	host := strings.ToLower(u.Hostname())
	if isLoopbackHost(host) || p.bypassed(host, strings.ToLower(u.Host)) {
		return nil
	}
	if pu := p.byScheme[strings.ToLower(u.Scheme)]; pu != nil {
		return pu
	}
	if p.all != nil {
		return p.all
	}
	return p.socks
}

// bypassed applies ProxyOverride. Patterns are matched against the host name
// and also against host:port, since an entry may name a port.
func (p *systemProxy) bypassed(host, hostport string) bool {
	if p.local && !strings.Contains(host, ".") && net.ParseIP(host) == nil {
		return true
	}
	for _, pat := range p.bypass {
		if wildcardMatch(pat, host) || wildcardMatch(pat, hostport) {
			return true
		}
	}
	return false
}

// isLoopbackHost is never sent through a proxy, whatever the settings say:
// the proxy's own loopback is not ours.
func isLoopbackHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// wildcardMatch reports whether s matches pat, where * stands for any run of
// characters, including none. Both are expected in lower case.
func wildcardMatch(pat, s string) bool {
	p, i := 0, 0
	star, mark := -1, 0
	for i < len(s) {
		switch {
		case p < len(pat) && pat[p] == '*':
			star, mark = p, i
			p++
		case p < len(pat) && pat[p] == s[i]:
			p++
			i++
		case star >= 0:
			// Let the last star swallow one more character and try again.
			mark++
			p, i = star+1, mark
		default:
			return false
		}
	}
	for p < len(pat) && pat[p] == '*' {
		p++
	}
	return p == len(pat)
}

// proxyEnvSet reports whether the environment names a proxy, in the variables
// Go itself reads.
func proxyEnvSet(getenv func(string) string) bool {
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if getenv(k) != "" {
			return true
		}
	}
	return false
}

// systemProxySettings is a variable so tests can stand in for the registry.
var systemProxySettings = readSystemProxy

// downloadProxy decides how one client's requests reach a server. It runs when
// the client is made, so a proxy switched on or off applies from the next
// download.
//
// A proxy named in the environment wins, as it does for command-line tools,
// and the environment then decides alone, NO_PROXY included. Otherwise the
// system's proxy setting applies.
func downloadProxy() func(*http.Request) (*url.URL, error) {
	if proxyEnvSet(os.Getenv) {
		return http.ProxyFromEnvironment
	}
	sys := systemProxySettings()
	if sys == nil {
		return nil
	}
	return func(r *http.Request) (*url.URL, error) { return sys.proxyFor(r.URL), nil }
}
