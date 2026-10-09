package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func proxyChoice(t *testing.T, p *systemProxy, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p == nil {
		return ""
	}
	if pu := p.proxyFor(u); pu != nil {
		return pu.String()
	}
	return ""
}

func TestSystemProxyServerForms(t *testing.T) {
	cases := []struct {
		name    string
		enabled bool
		server  string
		want    map[string]string // request -> proxy, "" for direct
	}{
		{"switched off", false, "127.0.0.1:7890", map[string]string{
			"https://example.com/f": "",
		}},
		{"switched on with nothing set", true, "", map[string]string{
			"https://example.com/f": "",
		}},
		{"one proxy for everything", true, "127.0.0.1:7890", map[string]string{
			"http://example.com/f":       "http://127.0.0.1:7890",
			"https://example.com:8443/f": "http://127.0.0.1:7890",
		}},
		{"a full URL for everything", true, "http://127.0.0.1:7890", map[string]string{
			"https://example.com/f": "http://127.0.0.1:7890",
		}},
		{"per scheme", true, "http=127.0.0.1:8080;https=127.0.0.1:8443", map[string]string{
			"http://example.com/f":  "http://127.0.0.1:8080",
			"https://example.com/f": "http://127.0.0.1:8443",
		}},
		{"http only leaves https direct", true, "http=proxy.lan:3128", map[string]string{
			"http://example.com/f":  "http://proxy.lan:3128",
			"https://example.com/f": "",
		}},
		{"socks only", true, "socks=127.0.0.1:1080", map[string]string{
			"http://example.com/f":  "socks5://127.0.0.1:1080",
			"https://example.com/f": "socks5://127.0.0.1:1080",
		}},
		{"socks covers schemes without their own", true, "http=h.lan:1;socks=s.lan:2", map[string]string{
			"http://example.com/f":  "http://h.lan:1",
			"https://example.com/f": "socks5://s.lan:2",
		}},
		{"spaces, case and stray separators", true, " HTTP = h.lan:1 ; HTTPS = h.lan:2 ;; ", map[string]string{
			"http://example.com/f":  "http://h.lan:1",
			"https://example.com/f": "http://h.lan:2",
		}},
		{"an https proxy named outright", true, "https=https://secure.lan:443", map[string]string{
			"https://example.com/f": "https://secure.lan:443",
		}},
		{"only protocols godm never uses", true, "ftp=f.lan:21", map[string]string{
			"https://example.com/f": "",
		}},
		{"an empty entry", true, "http=", map[string]string{
			"http://example.com/f": "",
		}},
	}
	for _, c := range cases {
		p := parseSystemProxy(c.enabled, c.server, "")
		for req, want := range c.want {
			if got := proxyChoice(t, p, req); got != want {
				t.Errorf("%s: %s went to %q, want %q", c.name, req, got, want)
			}
		}
	}
}

func TestSystemProxyBypassList(t *testing.T) {
	// The list Clash Verge writes, plus a wildcard name and one with a port.
	const override = "localhost;127.*;192.168.*;10.*;172.16.*;*.corp.example;" +
		"intranet.example:8080;HTTP://Legacy.Example;<local>"
	p := parseSystemProxy(true, "127.0.0.1:7897", override)
	const proxy = "http://127.0.0.1:7897"
	cases := map[string]string{
		"http://localhost:8080/":         "",
		"http://192.168.1.20/f":          "",
		"http://10.0.0.5/f":              "",
		"http://172.16.4.4/f":            "",
		"http://172.32.0.1/f":            proxy,
		"http://nas/share/f":             "", // <local>
		"https://files.corp.example/f":   "",
		"https://FILES.CORP.EXAMPLE/f":   "",
		"https://corp.example/f":         proxy, // *. needs something before the dot
		"http://intranet.example:8080/f": "",
		"http://intranet.example/f":      proxy,
		"http://legacy.example/f":        "",
		"https://example.com/f":          proxy,
		"http://[2001:db8::1]/f":         proxy, // an address is not a local name
	}
	for req, want := range cases {
		if got := proxyChoice(t, p, req); got != want {
			t.Errorf("%s went to %q, want %q", req, got, want)
		}
	}
}

func TestLoopbackIsNeverProxied(t *testing.T) {
	// No bypass list at all, and a proxy that would take everything.
	p := parseSystemProxy(true, "socks=127.0.0.1:1080;http=127.0.0.1:7890;https=127.0.0.1:7890", "")
	for _, req := range []string{
		"http://localhost/f", "http://LOCALHOST:16801/api/tasks", "http://127.0.0.1:16801/",
		"http://127.9.9.9/", "http://[::1]:8080/", "http://app.localhost/",
	} {
		if got := proxyChoice(t, p, req); got != "" {
			t.Errorf("%s went to %q; loopback must stay direct", req, got)
		}
	}
	if got := proxyChoice(t, p, "https://example.com/"); got == "" {
		t.Error("an ordinary host was not proxied, so the case above proves nothing")
	}
}

func TestWildcardMatch(t *testing.T) {
	cases := []struct {
		pat, s string
		want   bool
	}{
		{"*", "", true},
		{"*", "anything", true},
		{"", "", true},
		{"", "a", false},
		{"abc", "abc", true},
		{"abc", "abcd", false},
		{"a*c", "abc", true},
		{"a*c", "ac", true},
		{"a*c", "ab", false},
		{"*ab", "aab", true},
		{"a**b", "ab", true},
		{"*a*b*", "xxaxxbxx", true},
		{"*.x.com", "a.b.x.com", true},
		{"*.x.com", "x.com", false},
		{"192.168.*", "192.168.1.1", true},
		{"192.168.*", "192.169.1.1", false},
	}
	for _, c := range cases {
		if got := wildcardMatch(c.pat, c.s); got != c.want {
			t.Errorf("wildcardMatch(%q, %q) = %v, want %v", c.pat, c.s, got, c.want)
		}
	}
}

func TestProxyEnvSet(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want bool
	}{
		{map[string]string{}, false},
		{map[string]string{"NO_PROXY": "localhost"}, false}, // says who goes direct, names no proxy
		{map[string]string{"HTTPS_PROXY": "http://p:3128"}, true},
		{map[string]string{"http_proxy": "http://p:3128"}, true},
		{map[string]string{"HTTP_PROXY": ""}, false},
	}
	for _, c := range cases {
		if got := proxyEnvSet(func(k string) string { return c.env[k] }); got != c.want {
			t.Errorf("env %v: proxyEnvSet = %v, want %v", c.env, got, c.want)
		}
	}
}

func clearProxyEnv(t *testing.T) {
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		t.Setenv(k, "")
	}
}

func stubSystemProxy(t *testing.T, p *systemProxy) *atomic.Int32 {
	var reads atomic.Int32
	old := systemProxySettings
	systemProxySettings = func() *systemProxy { reads.Add(1); return p }
	t.Cleanup(func() { systemProxySettings = old })
	return &reads
}

func TestDownloadProxyChoosesBetweenEnvironmentAndSystem(t *testing.T) {
	clearProxyEnv(t)
	req, _ := http.NewRequest(http.MethodGet, "https://example.com/f", nil)

	stubSystemProxy(t, nil)
	if f := downloadProxy(); f != nil {
		t.Error("with no proxy anywhere the client should go direct")
	}

	reads := stubSystemProxy(t, parseSystemProxy(true, "127.0.0.1:7897", ""))
	f := downloadProxy()
	if f == nil {
		t.Fatal("the system proxy was ignored")
	}
	if u, _ := f(req); u == nil || u.String() != "http://127.0.0.1:7897" {
		t.Errorf("proxy = %v, want the system one", u)
	}

	// A proxy named in the environment decides alone, so the system setting
	// is not even read.
	t.Setenv("HTTPS_PROXY", "http://10.1.1.1:3128")
	before := reads.Load()
	downloadProxy()
	if reads.Load() != before {
		t.Error("the system proxy was consulted although the environment names one")
	}
}

// The settings are read when a client is made, so the engine really goes
// through the proxy, and switching it on applies to the next download.
func TestDownloadGoesThroughTheSystemProxy(t *testing.T) {
	clearProxyEnv(t)
	payload := makePayload(1 << 20)
	origin := &rangedHandler{payload: payload}
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A request sent to a proxy names the server it is meant for.
		if r.URL.Host != "files.example" {
			http.Error(w, "not a proxy request", http.StatusBadGateway)
			return
		}
		proxied.Add(1)
		origin.ServeHTTP(w, r)
	}))
	defer proxy.Close()
	stubSystemProxy(t, parseSystemProxy(true, strings.TrimPrefix(proxy.URL, "http://"), "<local>"))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := Download(ctx, Options{
		URL: "http://files.example/f.bin", OutDir: t.TempDir(), Connections: 4, MinSplit: 64 << 10,
	})
	if err != nil {
		t.Fatalf("download through the system proxy: %v", err)
	}
	checkFile(t, res.Path, payload)
	if n := proxied.Load(); n < 2 {
		t.Errorf("the proxy saw %d requests; the probe and the ranges should all pass through it", n)
	}
}
