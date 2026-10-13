package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// fakeDaemon stands in for the daemon's HTTP API and records what reached it.
func fakeDaemon(t *testing.T, handler http.HandlerFunc) (*daemonClient, *http.Request) {
	t.Helper()
	var last http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last = *r
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return &daemonClient{base: srv.URL, token: "tok", http: srv.Client()}, &last
}

// The confirmation dialog asks for a folder through the host. The chosen path
// has to come back, and the starting folder has to survive the query string
// untouched even when it has spaces and non-ASCII characters in it.
func TestNativeBrowseReturnsThePickedFolder(t *testing.T) {
	const start = `D:\Dow nloads & more\视频`
	c, last := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "path": `E:\Movies`})
	})

	resp := forwardNative(c, nativeRequest{Type: "browse", Current: start})

	if !resp.OK || resp.Error != "" || resp.Path != `E:\Movies` {
		t.Fatalf("got %+v", resp)
	}
	if last.Method != http.MethodPost || last.URL.Path != "/api/browse" {
		t.Errorf("reached the daemon as %s %s", last.Method, last.URL.Path)
	}
	if got := last.URL.Query().Get("current"); got != start {
		t.Errorf("starting folder arrived as %q", got)
	}
	if got := last.Header.Get("Authorization"); got != "Bearer tok" {
		t.Errorf("authorization header was %q", got)
	}
}

// Closing the chooser is not a failure: the dialog keeps what the user had.
func TestNativeBrowseCancelledIsOKWithNoPath(t *testing.T) {
	c, _ := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "path": ""})
	})
	resp := forwardNative(c, nativeRequest{Type: "browse"})
	if !resp.OK || resp.Path != "" || resp.Error != "" {
		t.Fatalf("got %+v", resp)
	}
	b, _ := json.Marshal(resp)
	var wire map[string]any
	_ = json.Unmarshal(b, &wire)
	if _, has := wire["path"]; has {
		t.Errorf("an empty path should be left off the wire, got %s", b)
	}
}

// The daemon answers 200 with ok:false when the chooser cannot be shown (one
// is already open, or there is no window). That must reach the dialog as an
// error with the daemon's reason, not as a silent empty pick.
func TestNativeBrowseReportsTheDaemonsReason(t *testing.T) {
	c, _ := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": false, "error": "a folder chooser is already open"})
	})
	resp := forwardNative(c, nativeRequest{Type: "browse"})
	if resp.OK || resp.Error != "a folder chooser is already open" {
		t.Fatalf("got %+v", resp)
	}
}

// watchForeground replaces the step that lets the daemon take the foreground
// with one that records the process it was asked about.
func watchForeground(t *testing.T, events *[]string, err error) {
	t.Helper()
	saved := allowForeground
	allowForeground = func(pid int) error {
		*events = append(*events, "allow "+strconv.Itoa(pid))
		return err
	}
	t.Cleanup(func() { allowForeground = saved })
}

// Chrome is in front when the dialog's Browse button is pressed, and Windows
// does not let the daemon put its chooser in front of it. The host, Chrome's
// child, has to allow the daemon's process to do that, and has to do it
// before the chooser is asked for, or the chooser is already open behind the
// browser by the time the permission arrives.
func TestNativeBrowseAllowsTheDaemonToTakeTheForegroundFirst(t *testing.T) {
	var events []string
	watchForeground(t, &events, nil)
	c, _ := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ping":
			writeJSON(w, map[string]any{"ok": true, "version": "test", "pid": 4242})
		case "/api/browse":
			events = append(events, "browse")
			writeJSON(w, map[string]any{"ok": true, "path": `E:\Movies`})
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
		}
	})

	resp := forwardNative(c, nativeRequest{Type: "browse"})

	if !resp.OK || resp.Path != `E:\Movies` {
		t.Fatalf("got %+v", resp)
	}
	if want := []string{"allow 4242", "browse"}; !reflect.DeepEqual(events, want) {
		t.Errorf("order of events = %v, want %v", events, want)
	}
}

// The permission is only a nicety: if it cannot be given, or the daemon's pid
// cannot be learned, the chooser still has to open.
func TestNativeBrowseStillOpensTheChooserWhenForegroundCannotBeAllowed(t *testing.T) {
	tests := []struct {
		name       string
		ping       map[string]any
		allowErr   error
		wantEvents []string
	}{
		{"the call is refused", map[string]any{"ok": true, "pid": 4242}, errors.New("access denied"), []string{"allow 4242", "browse"}},
		{"the daemon says no pid", map[string]any{"ok": true}, nil, []string{"browse"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var events []string
			watchForeground(t, &events, tt.allowErr)
			c, _ := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/ping" {
					writeJSON(w, tt.ping)
					return
				}
				events = append(events, "browse")
				writeJSON(w, map[string]any{"ok": true, "path": `E:\Movies`})
			})
			resp := forwardNative(c, nativeRequest{Type: "browse"})
			if !resp.OK || resp.Path != `E:\Movies` || resp.Error != "" {
				t.Fatalf("got %+v", resp)
			}
			if !reflect.DeepEqual(events, tt.wantEvents) {
				t.Errorf("events = %v, want %v", events, tt.wantEvents)
			}
		})
	}
}

// Only the folder chooser needs the foreground. A download or a ping from the
// extension must not hand the daemon anything.
func TestNativeRequestsOtherThanBrowseLeaveTheForegroundAlone(t *testing.T) {
	var events []string
	watchForeground(t, &events, nil)
	c, _ := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "id": "t1", "pid": 4242, "tasks": []any{}})
	})
	for _, typ := range []string{"ping", "tasks", "download", "config", "inspect", "batch"} {
		forwardNative(c, nativeRequest{Type: typ, URL: "https://example.test/a.zip"})
	}
	if len(events) != 0 {
		t.Errorf("the foreground was allowed for a request that is not browse: %v", events)
	}
}

// The dialog picks the file name, folder and connection count, so all three
// have to make it from the extension's message into the daemon's job.
func TestNativeDownloadCarriesTheDialogsChoices(t *testing.T) {
	var job jobRequest
	c, _ := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&job); err != nil {
			t.Errorf("daemon got bad json: %v", err)
		}
		writeJSON(w, map[string]any{"ok": true, "id": "t1"})
	})

	var req nativeRequest
	wire := `{"type":"download","url":"https://example.test/a.zip","filename":"renamed.zip",
		"outDir":"E:\\Stuff","connections":16,"referrer":"https://example.test/"}`
	if err := json.Unmarshal([]byte(wire), &req); err != nil {
		t.Fatal(err)
	}
	resp := forwardNative(c, req)

	if !resp.OK || resp.ID != "t1" {
		t.Fatalf("got %+v", resp)
	}
	if job.Filename != "renamed.zip" || job.OutDir != `E:\Stuff` || job.Connections != 16 {
		t.Errorf("job lost the dialog's choices: %+v", job)
	}
}

// The token is what stands between the browser and the daemon's API. The host
// holds it so that the extension does not have to: nothing the host answers
// may carry it, in an address or anywhere else, because whatever the extension
// is told, the rest of the browser can come by.
func TestNothingTheHostAnswersCarriesTheToken(t *testing.T) {
	const secret = "SECRET-DAEMON-TOKEN"
	c, _ := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"ok": true, "id": "t1", "ids": []string{"t1"}, "pid": 4242, "path": `E:\Movies`,
			"tasks": []any{map[string]any{"id": "t1", "filename": "a.zip"}},
			"info":  map[string]any{"title": "a"}, "out_dir": `C:\Users\x\Downloads`,
		})
	})
	c.token = secret
	var events []string
	watchForeground(t, &events, nil)

	watchManager(t, &events, nil)

	for _, typ := range []string{"ping", "tasks", "download", "", "config", "browse", "inspect", "batch", "open", "no-such-type"} {
		resp := forwardNative(c, nativeRequest{
			Type: typ, URL: "https://example.test/a.zip",
			Items: []batchItem{{URL: "https://example.test/b.zip"}},
		})
		wire, err := json.Marshal(resp)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(wire), secret) {
			t.Errorf("the answer to %q hands the extension the token: %s", typ, wire)
		}
	}
}

// watchManager replaces the step that opens the manager with one that only
// records being asked.
func watchManager(t *testing.T, events *[]string, err error) {
	t.Helper()
	saved := openManager
	openManager = func() error {
		*events = append(*events, "open the manager")
		return err
	}
	t.Cleanup(func() { openManager = saved })
}

// The popup's link to the manager used to be given the page's address, token
// and all, to open as a tab. The host opens the manager itself now, and the
// daemon is not asked for anything on the way.
func TestNativeOpenShowsTheManagerItself(t *testing.T) {
	var events []string
	watchManager(t, &events, nil)
	c, _ := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
	})

	resp := forwardNative(c, nativeRequest{Type: "open"})

	if !resp.OK || resp.Error != "" {
		t.Fatalf("got %+v", resp)
	}
	if want := []string{"open the manager"}; !reflect.DeepEqual(events, want) {
		t.Errorf("events = %v, want %v", events, want)
	}
	if wire, _ := json.Marshal(resp); string(wire) != `{"ok":true}` {
		t.Errorf("the answer is %s, want only that it worked", wire)
	}
}

// When the manager cannot be opened the popup has to hear why, so that the
// link does not just do nothing.
func TestNativeOpenReportsWhyTheManagerDidNotOpen(t *testing.T) {
	var events []string
	watchManager(t, &events, errors.New("the executable is gone"))
	c, _ := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {})

	resp := forwardNative(c, nativeRequest{Type: "open"})

	if resp.OK || resp.Error != "the executable is gone" {
		t.Fatalf("got %+v", resp)
	}
}

// Nothing but the popup's link opens the manager.
func TestNativeRequestsOtherThanOpenLeaveTheManagerAlone(t *testing.T) {
	var events []string
	watchManager(t, &events, nil)
	watchForeground(t, &events, nil)
	c, _ := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "id": "t1", "tasks": []any{}})
	})
	for _, typ := range []string{"ping", "tasks", "download", "config", "inspect", "batch", "no-such-type"} {
		forwardNative(c, nativeRequest{Type: typ, URL: "https://example.test/a.zip"})
	}
	if len(events) != 0 {
		t.Errorf("the manager was opened by a request that did not ask for it: %v", events)
	}
}

// The extension takes a host it cannot reach to be down and keeps its hands off
// downloads for a minute. A daemon that was reached and said no to one job is
// not that, so the host marks those answers for it to tell them apart.
func TestNativeMarksWhatTheDaemonRefusedAndNothingElse(t *testing.T) {
	refuse := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { http.Error(w, body, status) }
	}
	tests := []struct {
		name     string
		req      nativeRequest
		daemon   http.HandlerFunc
		rejected bool
	}{
		{"a job it will not take", nativeRequest{Type: "download", URL: "blob:x"}, refuse(400, "only http and https URLs are supported"), true},
		{"a job that is in conflict", nativeRequest{Type: "download", URL: "https://example.test/a"}, refuse(409, "already there"), true},
		{"a stream it cannot read", nativeRequest{Type: "inspect", URL: "https://example.test/a.m3u8"}, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"ok": false, "error": "this stream is protected"})
		}, true},
		{"a chooser it cannot show", nativeRequest{Type: "browse"}, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"ok": false, "error": "a folder chooser is already open"})
		}, true},
		{"a batch with nothing it accepts", nativeRequest{Type: "batch", Items: []batchItem{{URL: "ftp://x"}}}, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"ok": false, "ids": []string{}, "errors": []string{"ftp://x: only http and https URLs are supported"}})
		}, true},
		{"a daemon that fails", nativeRequest{Type: "download", URL: "https://example.test/a"}, refuse(500, "boom"), false},
		{"a token it does not know", nativeRequest{Type: "download", URL: "https://example.test/a"}, refuse(401, "unauthorized"), false},
		{"an address it does not serve", nativeRequest{Type: "tasks"}, refuse(404, "not found"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := fakeDaemon(t, tt.daemon)
			resp := forwardNative(c, tt.req)
			if resp.OK || (resp.Error == "" && len(resp.Errors) == 0) {
				t.Fatalf("got %+v", resp)
			}
			if resp.Rejected != tt.rejected {
				t.Errorf("rejected = %v, want %v (%+v)", resp.Rejected, tt.rejected, resp)
			}
		})
	}

	// A daemon that cannot be reached at all is not a refusal either.
	c, _ := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {})
	c.base = "http://127.0.0.1:1"
	if resp := forwardNative(c, nativeRequest{Type: "download", URL: "https://example.test/a"}); resp.OK || resp.Rejected {
		t.Errorf("an unreachable daemon was reported as a refusal: %+v", resp)
	}
}

// The daemon says why it will not take a job, and the person who right-clicked
// should read that, not the status line it was sent with.
func TestNativeReportsTheReasonTheDaemonGave(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"a sentence", 400, "this magnet link has no BitTorrent v1 info hash (urn:btih)\n", "this magnet link has no BitTorrent v1 info hash (urn:btih)"},
		{"in Chinese", 400, "只支持 http 和 https 链接\n", "只支持 http 和 https 链接"},
		{"nothing said", 400, "", "daemon returned 400 Bad Request"},
		{"only blanks", 400, " \r\n", "daemon returned 400 Bad Request"},
		{"a fault of its own", 500, "runtime error: index out of range\n", "daemon returned 500 Internal Server Error"},
		{"a token it does not know", 401, "unauthorized\n", "daemon returned 401 Unauthorized"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body))
			})
			for _, typ := range []string{"download", "batch", "tasks", "config"} {
				if resp := forwardNative(c, nativeRequest{Type: typ, URL: "https://example.test/a"}); resp.Error != tt.want {
					t.Errorf("%s: error = %q, want %q", typ, resp.Error, tt.want)
				}
			}
		})
	}
}

// A page of HTML or a long dump is not something to put in a notification, nor
// to push through the host's reply.
func TestNativeKeepsTheDaemonsReasonShort(t *testing.T) {
	c, _ := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(strings.Repeat("汉", 100000)))
	})
	resp := forwardNative(c, nativeRequest{Type: "download", URL: "https://example.test/a"})
	if n := len(resp.Error); n == 0 || n > 1200 {
		t.Fatalf("the reason is %d bytes", n)
	}
	if !utf8.ValidString(resp.Error) {
		t.Errorf("the reason was cut in the middle of a character: %q", resp.Error[len(resp.Error)-8:])
	}
	if !strings.HasPrefix(resp.Error, "汉汉") {
		t.Errorf("the reason does not start with what the daemon said: %.20q", resp.Error)
	}
}

// The daemon lets yt-dlp or a playlist fetch take up to 30 seconds. The host's
// ordinary short wait would give up on a slow site long before that, with a
// timeout error for an answer that was on its way.
func TestNativeInspectWaitsLongerThanOtherRequests(t *testing.T) {
	c, _ := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		writeJSON(w, map[string]any{"ok": true, "info": map[string]any{"title": "slow"}, "tasks": []any{}})
	})
	c.http.Timeout = 50 * time.Millisecond

	if resp := forwardNative(c, nativeRequest{Type: "tasks"}); resp.OK {
		t.Fatalf("the ordinary wait did not apply to the control request: %+v", resp)
	}
	resp := forwardNative(c, nativeRequest{Type: "inspect", Kind: "yt-dlp", URL: "https://example.test/watch"})
	if !resp.OK || resp.Info == nil || resp.Info.Title != "slow" {
		t.Fatalf("got %+v", resp)
	}
}

func TestNativeConfigPassesTheDefaultsThrough(t *testing.T) {
	c, last := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "out_dir": `C:\Users\x\Downloads`, "max_connections": 32})
	})
	resp := forwardNative(c, nativeRequest{Type: "config"})
	if !resp.OK || resp.Config["out_dir"] != `C:\Users\x\Downloads` || resp.Config["max_connections"] != float64(32) {
		t.Fatalf("got %+v", resp)
	}
	if last.URL.Path != "/api/config" {
		t.Errorf("reached the daemon at %s", last.URL.Path)
	}
}
