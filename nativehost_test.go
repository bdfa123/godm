package main

import (
	"bytes"
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

// sentBatch forwards a batch and returns the jobs that reached the daemon.
func sentBatch(t *testing.T, req nativeRequest) []jobRequest {
	t.Helper()
	var body struct {
		Items []jobRequest `json:"items"`
	}
	c, last := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("daemon got bad json: %v", err)
		}
		ids := make([]string, len(body.Items))
		for i := range ids {
			ids[i] = "t" + strconv.Itoa(i)
		}
		writeJSON(w, map[string]any{"ok": true, "ids": ids})
	})
	req.Type = "batch"
	resp := forwardNative(c, req)
	if !resp.OK || len(resp.IDs) != len(req.Items) {
		t.Fatalf("got %+v", resp)
	}
	if last.URL.Path != "/api/batch" {
		t.Errorf("reached the daemon at %s", last.URL.Path)
	}
	return body.Items
}

// The picker lists the streams a page played beside its links, and sends them
// in the same batch. A stream has to reach the daemon as one: a playlist served
// from a plain address like /play?id=7 is not known by its name, and would be
// saved as a text file. The files in the same batch stay files, each with the
// cookies of its own host, and the stream asks for no quality: the daemon takes
// the best.
func TestNativeBatchCarriesAStreamsKind(t *testing.T) {
	var req nativeRequest
	wire := `{"type":"batch","referrer":"https://video.test/watch/1","connections":4,"items":[
		{"url":"https://files.test/a.zip","filename":"","cookie":"a=1"},
		{"url":"https://cdn.test/play?id=7","filename":"The Film","cookie":"b=2","kind":"hls"},
		{"url":"https://files.test/b.zip","kind":""}]}`
	if err := json.Unmarshal([]byte(wire), &req); err != nil {
		t.Fatal(err)
	}

	jobs := sentBatch(t, req)

	if len(jobs) != 3 {
		t.Fatalf("%d jobs reached the daemon, want 3", len(jobs))
	}
	wantKind := []string{"", "hls", ""}
	for i, j := range jobs {
		if j.Kind != wantKind[i] {
			t.Errorf("item %d (%s) reached the daemon as kind %q, want %q", i, j.URL, j.Kind, wantKind[i])
		}
		if j.Variant != -1 {
			t.Errorf("item %d asked for variant %d, want -1, the best the playlist has", i, j.Variant)
		}
		if j.Connections != 4 || j.Referrer != "https://video.test/watch/1" || j.Headers["Referer"] != "https://video.test/watch/1" {
			t.Errorf("item %d lost what the batch shares: %+v", i, j)
		}
	}
	if jobs[1].Filename != "The Film" || jobs[1].Headers["Cookie"] != "b=2" || jobs[0].Headers["Cookie"] != "a=1" {
		t.Errorf("the items did not keep their own names and cookies: %+v", jobs)
	}
}

// The picker reads a stream before it offers it, as the popup does, and sends
// the quality that is best, by the number the playlist gave it. Sent as a
// number, it is the same request the popup makes of the same stream, which the
// daemon then knows for the same download; sent as "the best" it would not be,
// and the stream would be downloaded twice. A number is only the quality of a
// stream: on a file, or on the message as a whole, it means nothing, and a
// number that is not one is the best.
func TestNativeBatchStreamKeepsTheQualityItWasGiven(t *testing.T) {
	two, zero, minus := 2, 0, -3
	jobs := sentBatch(t, nativeRequest{
		Variant: &two, // the message's own, which must not become every item's
		Items: []batchItem{
			{URL: "https://v.test/a", Kind: "hls", Variant: &two},
			{URL: "https://v.test/b", Kind: "hls", Variant: &zero},
			{URL: "https://v.test/c", Kind: "hls"},
			{URL: "https://v.test/d", Kind: "hls", Variant: &minus},
			{URL: "https://v.test/e.zip", Variant: &two},
			{URL: "https://v.test/f", Kind: "bt", Variant: &two},
		},
	})
	var got []int
	for _, j := range jobs {
		got = append(got, j.Variant)
	}
	if want := []int{2, 0, -1, -1, -1, -1}; !reflect.DeepEqual(got, want) {
		t.Errorf("qualities that reached the daemon = %v, want %v", got, want)
	}
}

// The point of that number: a stream the popup is already downloading, and
// picks the best quality of by its number, is the same download when the picker
// is asked for it, and is not made a second time with a numbered name. One sent
// without a number, as the picker does for a stream it has not read, is not
// known to be the same, since only reading the playlist says which is best.
func TestAStreamFromThePickerIsTheSameDownloadAsFromThePopup(t *testing.T) {
	const stream = "https://cdn.test/hls/master.m3u8"
	best := 1
	popup := nativeRequest{Type: "download", URL: stream, Kind: "hls", Variant: &best}.job(stream, "The Film", "")
	dir := t.TempDir()
	running := &managedTask{req: popup, outDir: dir, view: TaskView{State: StateRunning}}

	jobs := sentBatch(t, nativeRequest{Items: []batchItem{
		{URL: stream, Kind: "hls", Variant: &best},
		{URL: stream, Kind: "hls"},
	}})
	if !sameActiveDownload(running, jobs[0], dir) {
		t.Errorf("the picker's request (%+v) is not the popup's (%+v), and the stream would be downloaded twice", jobs[0], popup)
	}
	if sameActiveDownload(running, jobs[1], dir) {
		t.Errorf("a request that names no quality was taken for the popup's, which names the best")
	}
}

// A batch may hold files and streams and nothing else. The extension can ask
// for a torrent or a yt-dlp download in a message of their own; a batch item
// that claims to be one is only a link, and neither a kind named on the
// message as a whole nor one an item names in other letters gets past that.
func TestNativeBatchPassesOnNoKindButHLS(t *testing.T) {
	jobs := sentBatch(t, nativeRequest{
		Kind: "bt", // the message's own, which must not turn the whole batch into torrents
		Items: []batchItem{
			{URL: "https://t.test/get?id=1", Kind: "bt"},
			{URL: "https://v.test/watch", Kind: "yt-dlp"},
			{URL: "https://v.test/x.m3u8", Kind: " HLS "},
			{URL: "https://v.test/y", Kind: "hls"},
			{URL: "https://v.test/z", Kind: "something new"},
			{URL: "https://v.test/w"},
		},
	})
	var got []string
	for _, j := range jobs {
		got = append(got, j.Kind)
	}
	if want := []string{"", "", "hls", "hls", "", ""}; !reflect.DeepEqual(got, want) {
		t.Errorf("kinds that reached the daemon = %q, want %q", got, want)
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

// bigList is what the daemon answers once a long run of downloads has piled up:
// n finished tasks, each with the signed address and the path it really has,
// and a running one at the front that is fetching in sixteen pieces.
func bigList(n int) []TaskView {
	long := "https://cdn.example.test/dl/" + strings.Repeat("sig=abcdef0123456789&", 25)
	tasks := []TaskView{{
		ID: "run", URL: long, Filename: "big.iso", Path: `D:\Downloads\big.iso`, State: "running",
		Size: 5 << 30, Received: 1 << 30, Speed: 9 << 20, Conns: 16, Active: 16,
		Segments: make([]SegmentView, 16),
	}}
	for i := 0; i < n; i++ {
		tasks = append(tasks, TaskView{
			ID: "t" + strconv.Itoa(i), URL: long, Referrer: "https://example.test/page/" + strconv.Itoa(i),
			Filename: "file" + strconv.Itoa(i) + ".zip", Path: `D:\Downloads\file` + strconv.Itoa(i) + ".zip",
			State: "done", Size: 1 << 20, Received: 1 << 20, Conns: 8,
		})
	}
	return tasks
}

// Chrome closes the host for a message over 1 MB, which then looks like the host
// being gone. The popup shows a handful of downloads, so that is all it is sent,
// however long the list has grown.
func TestNativeTasksReplyIsBoundedWhateverTheListHolds(t *testing.T) {
	for _, n := range []int{0, 5, 3000} {
		c, _ := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"ok": true, "tasks": bigList(n)})
		})
		resp := forwardNative(c, nativeRequest{Type: "tasks"})
		wire, _ := json.Marshal(resp)
		if !resp.OK || len(wire) > 64<<10 {
			t.Errorf("%d tasks: ok=%v, the reply is %d bytes", n, resp.OK, len(wire))
		}
		if want := min(n+1, popupTaskLimit); len(resp.Tasks) != want {
			t.Errorf("%d tasks: the popup is sent %d, want %d", n, len(resp.Tasks), want)
		}
	}
}

// What the popup draws has to survive the trimming, in the order it came.
func TestNativeTasksReplyKeepsWhatThePopupShows(t *testing.T) {
	list := bigList(20)
	c, _ := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "tasks": list})
	})
	resp := forwardNative(c, nativeRequest{Type: "tasks"})
	if len(resp.Tasks) < 2 {
		t.Fatalf("got %+v", resp)
	}
	for i, got := range resp.Tasks {
		want := list[i]
		if got.ID != want.ID || got.Filename != want.Filename || got.State != want.State ||
			got.Size != want.Size || got.Received != want.Received || got.Speed != want.Speed || got.Active != want.Active {
			t.Errorf("task %d changed: got %+v, want %+v", i, got, want)
		}
		if (len(got.Segments) > 1) != (len(want.Segments) > 1) {
			t.Errorf("task %d: the popup tells a download in pieces from one in a piece by this; %d became %d", i, len(want.Segments), len(got.Segments))
		}
		if !strings.HasPrefix(want.URL, got.URL) || got.URL == "" {
			t.Errorf("task %d: the address was changed, not shortened: %q", i, got.URL)
		}
	}
	if len(resp.Tasks[0].Segments) < 2 {
		t.Errorf("the running download lost its pieces: %+v", resp.Tasks[0].Segments)
	}
}

// Whatever else grows, a message the browser would drop must not be sent as if
// it were fine: the popup is told what happened instead.
func TestNativeMessageTooBigForTheBrowserIsReplacedByAnError(t *testing.T) {
	var buf bytes.Buffer
	if err := writeNativeMessage(&buf, nativeResponse{OK: true, Path: strings.Repeat("x", maxHostReply)}); err != nil {
		t.Fatal(err)
	}
	if buf.Len() > maxHostReply {
		t.Fatalf("wrote %d bytes, over what the browser takes", buf.Len())
	}
	got, err := readNativeMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	var resp nativeResponse
	if err := json.Unmarshal(got, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.Error == "" {
		t.Errorf("got %+v", resp)
	}

	buf.Reset()
	if err := writeNativeMessage(&buf, nativeResponse{OK: true, ID: "t1"}); err != nil {
		t.Fatal(err)
	}
	got, _ = readNativeMessage(&buf)
	if string(got) != `{"ok":true,"id":"t1"}` {
		t.Errorf("an ordinary answer was changed: %s", got)
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

// "Add to list" in the dialog, the menu and the picker sends later, which has
// to reach the daemon for one download and for every link of a batch; a
// message without it, as every other one is, starts at once.
func TestNativeLaterReachesTheDaemon(t *testing.T) {
	var got jobRequest
	c, last := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("daemon got bad json: %v", err)
		}
		writeJSON(w, map[string]any{"ok": true, "id": "t1"})
	})
	for _, later := range []bool{true, false} {
		got = jobRequest{}
		var req nativeRequest
		wire := `{"type":"download","url":"https://files.test/a.zip","later":` + strconv.FormatBool(later) + `}`
		if err := json.Unmarshal([]byte(wire), &req); err != nil {
			t.Fatal(err)
		}
		if resp := forwardNative(c, req); !resp.OK || last.URL.Path != "/api/download" {
			t.Fatalf("got %+v at %s", resp, last.URL.Path)
		}
		if got.Later != later {
			t.Errorf("a download sent with later %v reached the daemon with %v", later, got.Later)
		}
	}

	var req nativeRequest
	wire := `{"later":true,"items":[{"url":"https://files.test/a.zip"},{"url":"https://cdn.test/p","kind":"hls"}]}`
	if err := json.Unmarshal([]byte(wire), &req); err != nil {
		t.Fatal(err)
	}
	for i, j := range sentBatch(t, req) {
		if !j.Later {
			t.Errorf("item %d of a batch for the list was sent to start at once", i)
		}
	}
}
