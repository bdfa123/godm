package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

// accessAPI serves everything the daemon serves, with a token long enough to
// be told apart from anything else in the page.
func accessAPI(t *testing.T) (*server, *httptest.Server) {
	t.Helper()
	s := &server{mgr: NewManager(t.TempDir(), 2), token: "secret-token-0123456789abcdef"}
	api := httptest.NewServer(s.routes())
	t.Cleanup(api.Close)
	return s, api
}

// ask sends one request and returns the status and the body. host, when set,
// is what the request claims to be addressed to.
func ask(t *testing.T, method, target, host string, header map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// The page carries the token, so it is the token: whoever is given the page
// can call the whole API. Another account on the same PC can reach loopback
// but cannot read the token file, and must not be handed its contents here.
func TestThePageIsOnlyGivenToACallerWhoShowsTheToken(t *testing.T) {
	s, api := accessAPI(t)

	for name, target := range map[string]string{
		"no token":      api.URL + "/",
		"a wrong token": api.URL + "/?token=" + strings.Repeat("0", len(s.token)),
		"an empty one":  api.URL + "/?token=",
	} {
		code, body := ask(t, http.MethodGet, target, "", nil)
		if code != http.StatusUnauthorized {
			t.Errorf("%s: GET / answered %d, want 401", name, code)
		}
		if strings.Contains(body, s.token) {
			t.Errorf("%s: GET / handed out the token", name)
		}
	}
	if code, body := ask(t, http.MethodGet, api.URL+"/", "", map[string]string{"Authorization": "Bearer nope"}); code != http.StatusUnauthorized || strings.Contains(body, s.token) {
		t.Errorf("GET / with a wrong bearer token answered %d", code)
	}

	// What godm app, godm ui and the tray open.
	code, body := ask(t, http.MethodGet, api.URL+"/?token="+s.token, "", nil)
	if code != http.StatusOK || !strings.Contains(body, `var TOKEN = "`+s.token+`";`) {
		t.Errorf("GET / with the token answered %d and the page %v the token", code,
			map[bool]string{true: "carries", false: "lacks"}[strings.Contains(body, s.token)])
	}
	// The token in a header is as good as one in the address.
	code, body = ask(t, http.MethodGet, api.URL+"/", "", map[string]string{"Authorization": "Bearer " + s.token})
	if code != http.StatusOK || !strings.Contains(body, s.token) {
		t.Errorf("GET / with a bearer token answered %d", code)
	}
}

// A web page that points its own name at 127.0.0.1 becomes the same origin as
// the daemon in the browser's eyes and can read what it answers. The request
// still says which name it was sent to, and only the daemon's own will do.
func TestARequestAddressedToAnotherNameIsRefused(t *testing.T) {
	s, api := accessAPI(t)
	port := api.URL[strings.LastIndex(api.URL, ":")+1:]
	auth := map[string]string{"Authorization": "Bearer " + s.token}

	for _, host := range []string{
		"attacker.example:" + port,
		"attacker.example",
		"127.0.0.1.attacker.example:" + port,
		"localhost.attacker.example:" + port,
		"127.0.0.1",   // no port: not how anything reaches a daemon off port 80
		"127.0.0.1:1", // another program's port
		"localhost:1",
		"[::1]:" + port, // the daemon does not listen there
		"0.0.0.0:" + port,
		"127.0.0.2:" + port,
	} {
		if code, body := ask(t, http.MethodGet, api.URL+"/?token="+s.token, host, nil); code != http.StatusForbidden || strings.Contains(body, s.token) {
			t.Errorf("Host %q: GET / with the token answered %d (token in the answer: %v), want 403",
				host, code, strings.Contains(body, s.token))
		}
		if code, _ := ask(t, http.MethodGet, api.URL+"/api/tasks", host, auth); code != http.StatusForbidden {
			t.Errorf("Host %q: GET /api/tasks with the token answered %d, want 403", host, code)
		}
		if code, _ := ask(t, http.MethodPost, api.URL+"/api/pause-all", host, auth); code != http.StatusForbidden {
			t.Errorf("Host %q: POST /api/pause-all with the token answered %d, want 403", host, code)
		}
		// A preflight is answered before the token is looked at, so it has to
		// be turned away as well, or it would tell a rebound page to go ahead.
		pre := map[string]string{"Origin": "chrome-extension://abcdefghijklmnop", "Access-Control-Request-Method": "POST"}
		if code, _ := ask(t, http.MethodOptions, api.URL+"/api/pause-all", host, pre); code != http.StatusForbidden {
			t.Errorf("Host %q: OPTIONS /api/pause-all answered %d, want 403", host, code)
		}
	}

	// Both of the daemon's own names keep working, however the name is cased.
	for _, host := range []string{"127.0.0.1:" + port, "localhost:" + port, "LocalHost:" + port} {
		if code, _ := ask(t, http.MethodGet, api.URL+"/?token="+s.token, host, nil); code != http.StatusOK {
			t.Errorf("Host %q: GET / with the token answered %d, want 200", host, code)
		}
		if code, _ := ask(t, http.MethodGet, api.URL+"/api/tasks", host, auth); code != http.StatusOK {
			t.Errorf("Host %q: GET /api/tasks with the token answered %d, want 200", host, code)
		}
	}
}

// Without a listener behind the request, as when a handler is called directly,
// the port to match is the one the daemon says it has.
func TestTheHostHasToNameTheDaemonsOwnPort(t *testing.T) {
	s := &server{mgr: NewManager(t.TempDir(), 2), token: "tok", port: 16801}
	for host, want := range map[string]bool{
		"127.0.0.1:16801":        true,
		"localhost:16801":        true,
		"127.0.0.1:16802":        false,
		"127.0.0.1:016801":       false,
		"127.0.0.1":              false,
		"":                       false,
		"example.com":            false,
		"localhost.:16801":       false,
		"127.0.0.1:16801:16801":  false,
		"user@127.0.0.1:16801":   false,
		"attacker.example:16801": false,
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
		r.Host = host
		if got := s.addressedToUs(r); got != want {
			t.Errorf("Host %q: taken as ours = %v, want %v", host, got, want)
		}
	}
}

// Every address the daemon answers at has to ask for the token. The list is
// read out of the source so that one added later is held to the same thing
// without anybody remembering to add it here.
func TestNothingAnswersWithoutTheToken(t *testing.T) {
	src, err := os.ReadFile("daemon.go")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, m := range regexp.MustCompile(`mux\.HandleFunc\("([^"]+)"`).FindAllStringSubmatch(string(src), -1) {
		paths = append(paths, m[1])
	}
	if len(paths) < 20 {
		t.Fatalf("found only %d addresses in daemon.go: %v", len(paths), paths)
	}

	s, api := accessAPI(t)
	for _, p := range paths {
		if strings.HasSuffix(p, "/") && p != "/" {
			p += "anything"
		}
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			code, body := ask(t, method, api.URL+p, "", nil)
			if code != http.StatusUnauthorized {
				t.Errorf("%s %s without a token answered %d, want 401", method, p, code)
			}
			if strings.Contains(body, s.token) {
				t.Errorf("%s %s without a token handed it out", method, p)
			}
		}
	}
	// Paths nothing is registered for fall to the page's handler.
	for _, p := range []string{"/index.html", "/favicon.ico", "/api/", "/api/nothing"} {
		if code, body := ask(t, http.MethodGet, api.URL+p, "", nil); code != http.StatusNotFound || strings.Contains(body, s.token) {
			t.Errorf("GET %s answered %d, want 404", p, code)
		}
	}
}

// The page takes the token out of its address once it has loaded, so that it
// does not stay in the tab's history. Reloading then asks for / with nothing,
// and what comes back has to find the way to the page again without holding
// anything secret itself.
func TestAReloadedPageFindsItsWayBack(t *testing.T) {
	s, api := accessAPI(t)

	_, page := ask(t, http.MethodGet, api.URL+"/?token="+s.token, "", nil)
	for _, want := range []string{
		`sessionStorage.setItem("godm-token", TOKEN)`,
		`history.replaceState(null, "", location.pathname)`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not run %s", want)
		}
	}

	code, locked := ask(t, http.MethodGet, api.URL+"/", "", nil)
	if code != http.StatusUnauthorized || strings.Contains(locked, s.token) {
		t.Fatalf("GET / with no token answered %d (token in the answer: %v)", code, strings.Contains(locked, s.token))
	}
	for _, want := range []string{
		`sessionStorage.getItem("godm-token")`,
		`location.replace("/?token=" + encodeURIComponent(kept))`,
		"godm ui",
	} {
		if !strings.Contains(locked, want) {
			t.Errorf("the answer to a caller with no token lacks %s:\n%s", want, locked)
		}
	}
	if strings.Contains(locked, "__WHY__") {
		t.Errorf("a placeholder is left in the answer:\n%s", locked)
	}

	// The kept token can be out of date: the token file was deleted and the
	// daemon made a new one. Sending the tab back and forth would never end,
	// so a token that is refused gets an answer with no script in it.
	_, refused := ask(t, http.MethodGet, api.URL+"/?token=stale", "", nil)
	if strings.Contains(refused, "<script") || strings.Contains(refused, "location.replace") {
		t.Errorf("a refused token is answered with a page that would try again:\n%s", refused)
	}

	setLanguage(t, s.mgr, langZH)
	if _, locked = ask(t, http.MethodGet, api.URL+"/", "", nil); !strings.Contains(locked, "通知区域") {
		t.Errorf("the answer is not in the chosen language:\n%s", locked)
	}
}
