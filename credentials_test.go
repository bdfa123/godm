package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// headerLog keeps the headers of every request a test server was sent.
type headerLog struct {
	mu   sync.Mutex
	seen []http.Header
}

func (l *headerLog) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		l.seen = append(l.seen, r.Header.Clone())
		l.mu.Unlock()
		h.ServeHTTP(w, r)
	})
}

func (l *headerLog) all() []http.Header {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]http.Header(nil), l.seen...)
}

// asOtherHost names a test server by "localhost" instead of 127.0.0.1. It is
// the same listener, but to anything that compares host names, as a browser
// does before it sends a cookie, it is a different site.
func asOtherHost(t *testing.T, srvURL string) string {
	t.Helper()
	if !strings.Contains(srvURL, "//127.0.0.1:") {
		t.Fatalf("test server is at %s, expected 127.0.0.1", srvURL)
	}
	return strings.Replace(srvURL, "//127.0.0.1:", "//localhost:", 1)
}

// siteLogin is what a browser would hand over for the site a link was clicked
// on, together with two headers that say nothing about who the user is.
func siteLogin() map[string]string {
	return map[string]string{
		"Cookie":        "session=SITE-A-SESSION",
		"Authorization": "Bearer SITE-A-TOKEN",
		"User-Agent":    "godm-test-agent",
		"Referer":       "http://site.example/page",
	}
}

// checkNoLogin fails for every request in reqs that carried the site's login,
// and for every one that lost the headers that may go anywhere.
func checkNoLogin(t *testing.T, who string, reqs []http.Header) {
	t.Helper()
	for _, h := range reqs {
		for _, name := range []string{"Cookie", "Authorization"} {
			if v := h.Get(name); v != "" {
				t.Errorf("%s was sent %s: %q (Range %q): it belongs to the site the link was on",
					who, name, v, h.Get("Range"))
			}
		}
		if h.Get("User-Agent") != "godm-test-agent" || h.Get("Referer") != "http://site.example/page" {
			t.Errorf("%s got User-Agent %q and Referer %q: those are no secret and should still be sent",
				who, h.Get("User-Agent"), h.Get("Referer"))
		}
	}
}

func checkLogin(t *testing.T, who string, reqs []http.Header) {
	t.Helper()
	for _, h := range reqs {
		if h.Get("Cookie") != "session=SITE-A-SESSION" || h.Get("Authorization") != "Bearer SITE-A-TOKEN" {
			t.Errorf("%s got Cookie %q and Authorization %q (Range %q): its own login was left off",
				who, h.Get("Cookie"), h.Get("Authorization"), h.Get("Range"))
		}
	}
}

func TestWhereALoginMayGo(t *testing.T) {
	cases := []struct {
		origin, dest string
		want         bool
	}{
		{"https://site.example/dl?id=1", "https://site.example/files/f.bin", true},
		{"https://site.example/dl", "https://site.example:8443/f.bin", true}, // cookies ignore the port
		{"https://site.example/dl", "https://dl.site.example/f.bin", true},
		{"https://site.example/dl", "https://a.b.site.example/f.bin", true},
		{"https://Site.Example/dl", "https://DL.SITE.EXAMPLE./f.bin", true},
		{"http://site.example/dl", "http://site.example/f.bin", true},
		{"http://site.example/dl", "https://site.example/f.bin", true},
		{"http://127.0.0.1:8080/dl", "http://127.0.0.1:9090/f.bin", true},
		{"http://[::1]:8080/dl", "http://[::1]:9090/f.bin", true},

		{"https://site.example/dl", "https://cdn.example/f.bin", false},
		{"https://site.example/dl", "https://example/f.bin", false},
		{"https://dl.site.example/dl", "https://site.example/f.bin", false},     // up, not down
		{"https://dl.site.example/dl", "https://www.site.example/f.bin", false}, // a sibling
		{"https://site.example/dl", "https://evilsite.example/f.bin", false},    // ends the same, no dot
		{"https://site.example/dl", "https://site.example.evil.test/f.bin", false},
		{"https://site.example/dl", "https://site.example@evil.test/f.bin", false}, // a user name, not the host
		{"https://site.example/dl", "http://site.example/f.bin", false},            // same site, in the clear
		{"https://site.example/dl", "http://dl.site.example/f.bin", false},
		{"http://127.0.0.1:8080/dl", "http://localhost:8080/f.bin", false},
		{"http://127.0.0.1/dl", "http://evil.127.0.0.1/f.bin", false},
		{"http://[::1]/dl", "http://[::1%25.site.example]/f.bin", false},
		{"https://site.example/dl", "https:///f.bin", false},
		{"://not a url", "https://site.example/f.bin", false},
	}
	for _, c := range cases {
		req, err := http.NewRequest(http.MethodGet, c.dest, nil)
		if err != nil {
			t.Fatalf("%s: %v", c.dest, err)
		}
		applyHeaders(req, siteLogin(), c.origin)
		got := req.Header.Get("Cookie") != ""
		if got != (req.Header.Get("Authorization") != "") {
			t.Errorf("%s -> %s: Cookie and Authorization were treated differently", c.origin, c.dest)
		}
		if got != c.want {
			t.Errorf("%s -> %s: login sent = %v, want %v", c.origin, c.dest, got, c.want)
		}
		if req.Header.Get("User-Agent") != "godm-test-agent" || req.Header.Get("Referer") == "" {
			t.Errorf("%s -> %s: the headers that are no secret were dropped too", c.origin, c.dest)
		}
	}
}

// net/http follows the redirects of a request, and on each one it copies the
// first request's headers again. It leaves the login off when the redirect
// goes to another site; the client has to do the same when it goes from https
// to http on the same one.
func TestARedirectDoesNotCarryALoginOutOfItsSite(t *testing.T) {
	check := newClient().CheckRedirect
	if check == nil {
		t.Fatal("the download client lets net/http alone decide what a redirect carries")
	}
	first, _ := http.NewRequest(http.MethodGet, "https://site.example/dl?id=1", nil)
	hop := func(dest string) *http.Request {
		req, _ := http.NewRequest(http.MethodGet, dest, nil)
		applyHeaders(req, siteLogin(), dest) // as net/http copies them: all of them
		return req
	}
	for dest, want := range map[string]bool{
		"https://site.example/f.bin":    true,
		"https://dl.site.example/f.bin": true,
		"http://site.example/f.bin":     false,
		"http://dl.site.example/f.bin":  false,
		"https://cdn.example/f.bin":     false,
	} {
		req := hop(dest)
		if err := check(req, []*http.Request{first}); err != nil {
			t.Fatalf("%s: %v", dest, err)
		}
		if got := req.Header.Get("Cookie") != "" && req.Header.Get("Authorization") != ""; got != want {
			t.Errorf("redirect to %s: login sent = %v, want %v", dest, got, want)
		}
		if !want && (req.Header.Get("Cookie") != "" || req.Header.Get("Authorization") != "") {
			t.Errorf("redirect to %s kept part of the login: %v", dest, req.Header)
		}
		if req.Header.Get("User-Agent") != "godm-test-agent" {
			t.Errorf("redirect to %s lost its User-Agent", dest)
		}
	}

	// A CheckRedirect replaces net/http's own, and with it the limit that
	// stops two addresses redirecting to each other for ever.
	via := make([]*http.Request, 10)
	for i := range via {
		via[i] = first
	}
	if err := check(hop("https://site.example/f.bin"), via); err == nil {
		t.Error("an eleventh redirect was followed")
	}
}

// The same thing end to end, through net/http's own redirect handling: an
// https link whose server redirects to plain http on the same host. Nothing
// about the host changed, so net/http would send the cookie on in the clear.
func TestAnHTTPSLoginIsNotSentOnOverPlainHTTP(t *testing.T) {
	payload := makePayload(8 << 10)
	var plain, secure headerLog
	plainSrv := httptest.NewServer(plain.wrap(&rangedHandler{payload: payload}))
	defer plainSrv.Close()
	secureSrv := httptest.NewTLSServer(secure.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plainSrv.URL+"/f.bin", http.StatusFound)
	})))
	defer secureSrv.Close()

	client := newClient()
	client.Transport.(*http.Transport).TLSClientConfig = secureSrv.Client().Transport.(*http.Transport).TLSClientConfig
	o := Options{URL: secureSrv.URL + "/download?id=1", Headers: siteLogin()}
	pr, err := Probe(context.Background(), client, o)
	if err != nil {
		t.Fatal(err)
	}
	if pr.FinalURL != plainSrv.URL+"/f.bin" || pr.Size != int64(len(payload)) {
		t.Fatalf("probe ended at %s with %d bytes, want the redirect followed", pr.FinalURL, pr.Size)
	}
	checkLogin(t, "the https site", secure.all())
	if len(plain.all()) != 1 {
		t.Fatalf("the http address got %d requests, want the probe", len(plain.all()))
	}
	checkNoLogin(t, "the http address", plain.all())

	// And the requests that fetch the file, which go straight there.
	req, _ := http.NewRequest(http.MethodGet, pr.FinalURL, nil)
	applyHeaders(req, o.Headers, o.URL)
	checkNoLogin(t, "a range request to the http address", []http.Header{req.Header})
}

// A site that answers a download with a redirect to another host is how most
// file hosts and CDNs work, and how an open redirect is abused. The cookies
// were the browser's for the site, so the other host must never see them: not
// on the probe, not on any range request, not on a retry.
func TestALoginDoesNotFollowADownloadToAnotherHost(t *testing.T) {
	payload := makePayload(256 << 10)
	h := &rangedHandler{payload: payload, cutAfter: 1000}
	h.cutsLeft.Store(2) // two connections are cut short, so there are retries
	var other, site headerLog
	otherSrv := httptest.NewServer(other.wrap(h))
	defer otherSrv.Close()
	otherURL := asOtherHost(t, otherSrv.URL)
	siteSrv := httptest.NewServer(site.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, otherURL+"/f.bin", http.StatusFound)
	})))
	defer siteSrv.Close()

	res, err := Download(context.Background(), Options{
		URL: siteSrv.URL + "/download?id=1", OutDir: t.TempDir(),
		Connections: 4, MinSplit: 4 << 10, MaxRetries: 6, Headers: siteLogin(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(res.Path); sum(got) != sum(payload) {
		t.Fatal("content mismatch")
	}

	if len(site.all()) == 0 {
		t.Fatal("the site was never asked")
	}
	checkLogin(t, "the site", site.all())
	// The probe, four ranges and two retries at the least.
	if n := len(other.all()); n < 7 {
		t.Fatalf("the other host got %d requests, want the probe, the ranges and their retries", n)
	}
	checkNoLogin(t, "the other host", other.all())
}

// The same site on another port, or a redirect that stays on the site, is
// still the site: a download that needs its login there must keep working.
func TestALoginStaysWithADownloadOnTheSameHost(t *testing.T) {
	payload := makePayload(64 << 10)
	var files, site headerLog
	fileSrv := httptest.NewServer(files.wrap(&rangedHandler{payload: payload}))
	defer fileSrv.Close()
	siteSrv := httptest.NewServer(site.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fileSrv.URL+"/f.bin", http.StatusFound)
	})))
	defer siteSrv.Close()

	res, err := Download(context.Background(), Options{
		URL: siteSrv.URL + "/download?id=1", OutDir: t.TempDir(),
		Connections: 4, MinSplit: 4 << 10, Headers: siteLogin(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(res.Path); sum(got) != sum(payload) {
		t.Fatal("content mismatch")
	}
	checkLogin(t, "the site", site.all())
	if n := len(files.all()); n < 2 {
		t.Fatalf("the file server got %d requests, want the probe and at least one range", n)
	}
	checkLogin(t, "the site's file server", files.all())
}

// A playlist names its segments and its key by full address, and they are
// often on another host. Each of those requests is made directly, so nothing
// but the engine stands between the site's cookies and that host.
func TestALoginDoesNotFollowAStreamToAnotherHost(t *testing.T) {
	segs := makeSegments(4)
	key := []byte("0123456789abcdef")

	var other, site headerLog
	otherSrv := httptest.NewServer(other.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/key.bin":
			w.Write(key)
		case "/media.m3u8":
			// The variant playlist: the first segment back on the site, the
			// rest and the key here.
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n"+
				"#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n"+
				"#EXTINF:4.0,\n%s/seg0.ts\n", r.URL.Query().Get("site"))
			for i := 1; i < len(segs); i++ {
				fmt.Fprintf(w, "#EXTINF:4.0,\nseg%d.ts\n", i)
			}
			fmt.Fprint(w, "#EXT-X-ENDLIST\n")
		default:
			var i int
			if _, err := fmt.Sscanf(r.URL.Path, "/seg%d.ts", &i); err != nil || i < 1 || i >= len(segs) {
				http.NotFound(w, r)
				return
			}
			w.Write(encryptHLSSegment(segs[i], key, int64(i)))
		}
	})))
	defer otherSrv.Close()
	otherURL := asOtherHost(t, otherSrv.URL)

	var siteURL string
	siteSrv := httptest.NewServer(site.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/master.m3u8":
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360\n"+
				"%s/media.m3u8?site=%s\n", otherURL, siteURL)
		case "/seg0.ts":
			w.Write(encryptHLSSegment(segs[0], key, 0))
		default:
			http.NotFound(w, r)
		}
	})))
	defer siteSrv.Close()
	siteURL = siteSrv.URL

	target, err := DownloadHLS(context.Background(), HLSOptions{
		URL: siteURL + "/master.m3u8", OutDir: t.TempDir(), Filename: "clip.ts",
		Connections: 2, Variant: -1, Headers: siteLogin(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(target); !bytes.Equal(got, joined(segs)) {
		t.Fatal("the saved file does not match the stream")
	}

	// The master playlist and the one segment the site serves itself.
	if n := len(site.all()); n != 2 {
		t.Fatalf("the site got %d requests, want 2", n)
	}
	checkLogin(t, "the site", site.all())
	// The variant playlist, the key and three segments.
	if n := len(other.all()); n != 5 {
		t.Fatalf("the other host got %d requests, want 5", n)
	}
	checkNoLogin(t, "the other host", other.all())
}
