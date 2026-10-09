package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"
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
