package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestFirstMissingFollowsEachSegmentsRun(t *testing.T) {
	seg := func(start, end, done int64) *Segment {
		s := newSegment(start, end)
		s.done.Store(done)
		return s
	}
	tk := &task{segs: []*Segment{
		seg(0, 99, 100),   // complete
		seg(100, 199, 30), // 100..129 on disk
		seg(200, 299, 100),
		seg(300, 399, 0),
	}}
	cases := []struct{ off, want int64 }{
		{0, 130},   // runs through the complete first segment
		{120, 130}, // inside a partial run
		{150, 150}, // past the run: missing already
		{200, 300}, // a complete segment carries on into the next
		{399, 399},
		{400, 400}, // the end of the file
	}
	for _, c := range cases {
		if got := tk.firstMissingLocked(c.off); got != c.want {
			t.Errorf("firstMissing(%d) = %d, want %d", c.off, got, c.want)
		}
	}
}

func TestParseRangeHeader(t *testing.T) {
	const size = 1000
	cases := []struct {
		h                string
		start, end       int64
		partial, satisfy bool
	}{
		{"", 0, 999, false, true},
		{"bytes=0-", 0, 999, true, true},
		{"bytes=100-199", 100, 199, true, true},
		{"bytes=900-5000", 900, 999, true, true}, // clipped to the file
		{"bytes=-100", 900, 999, true, true},     // the last 100 bytes
		{"bytes=1000-", 0, 0, false, false},      // starts past the end
		{"bytes=5-1", 0, 999, false, true},       // nonsense is ignored
		{"bytes=0-1,5-6", 0, 999, false, true},   // several ranges: whole file
		{"items=0-5", 0, 999, false, true},
	}
	for _, c := range cases {
		s, e, p, ok := parseRangeHeader(c.h, size)
		if s != c.start || e != c.end || p != c.partial || ok != c.satisfy {
			t.Errorf("%q = %d-%d partial=%v ok=%v, want %d-%d partial=%v ok=%v",
				c.h, s, e, p, ok, c.start, c.end, c.partial, c.satisfy)
		}
	}
}

// A player reading near the end of a file must not wait for the connections
// to work their way there from wherever they started.
func TestPlaybackPullsConnectionsToTheReader(t *testing.T) {
	payload := makePayload(16 << 20)
	h := &slowServer{payload: payload, chunk: 16 << 10, delay: 10 * time.Millisecond}
	srv := httptest.NewServer(h)
	defer srv.Close()

	var pb Playback
	rd := pb.NewReader()
	const focus, want = 10 << 20, 2 << 20
	rd.Want(focus)

	done := make(chan error, 1)
	var res *Result
	go func() {
		var err error
		res, err = Download(context.Background(), Options{
			URL: srv.URL + "/film.bin", OutDir: t.TempDir(),
			Connections: 4, MinSplit: 64 << 10, StreamPiece: 256 << 10,
			Playback: &pb,
		})
		done <- err
	}()

	deadline := time.Now().Add(20 * time.Second)
	for {
		if n, ok := pb.Available(focus); ok && n >= want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the bytes the player wants never arrived")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Spread over the file, the four connections would have fetched nearly
	// all of it before the one working through this part got this far.
	if sent := h.total(); sent > 8<<20 {
		t.Fatalf("%d MiB came down before the %d MiB at the player did", sent>>20, want>>20)
	}

	rd.Close()
	if err := <-done; err != nil {
		t.Fatalf("download: %v", err)
	}
	checkFile(t, res.Path, payload)
}

// Seeking in a Matroska file starts with its index, a few hundred KB at the
// very end: less than a piece normally worth its own connection, and nothing
// plays until it arrives.
func TestPlayerWaitingOnTheLastBytesGetsAConnection(t *testing.T) {
	payload := makePayload(4 << 20)
	h := &slowServer{payload: payload, chunk: 16 << 10, delay: 10 * time.Millisecond}
	srv := httptest.NewServer(h)
	defer srv.Close()

	var pb Playback
	rd := pb.NewReader()
	const tail = 100 << 10
	focus := int64(len(payload) - tail)
	rd.Want(focus)

	done := make(chan error, 1)
	go func() {
		_, err := Download(context.Background(), Options{
			URL: srv.URL + "/film.mkv", OutDir: t.TempDir(),
			Connections: 2, MinSplit: 512 << 10, StreamPiece: 1 << 20,
			Playback: &pb,
		})
		done <- err
	}()

	deadline := time.Now().Add(20 * time.Second)
	for {
		if n, ok := pb.Available(focus); ok && n >= tail {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the end of the file never arrived")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if sent := h.total(); sent > 2<<20 {
		t.Fatalf("%d KiB came down before the last %d KiB the player was waiting on", sent>>10, tail>>10)
	}
	rd.Close()
	if err := <-done; err != nil {
		t.Fatalf("download: %v", err)
	}
}

func TestStreamServesARangeBeforeTheDownloadFinishes(t *testing.T) {
	payload := makePayload(8 << 20)
	src := httptest.NewServer(&slowServer{payload: payload, chunk: 8 << 10, delay: 20 * time.Millisecond})
	defer src.Close()

	m := NewManager(t.TempDir(), 2)
	m.streamPiece = 128 << 10
	s := &server{mgr: m, token: "tok"}
	mux := http.NewServeMux()
	mux.HandleFunc("/stream/", s.guard(s.handleStream))
	api := httptest.NewServer(mux)
	defer api.Close()

	id, err := m.Add(jobRequest{URL: src.URL + "/film.mkv", Connections: 4})
	if err != nil {
		t.Fatal(err)
	}
	link := api.URL + "/stream/" + id + "/film.mkv?token=tok"

	const from, to = 5 << 20, 6<<20 - 1
	req, _ := http.NewRequest(http.MethodGet, link, nil)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", from, to))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status %d, want 206", resp.StatusCode)
	}
	if got, want := resp.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-%d/%d", from, to, len(payload)); got != want {
		t.Errorf("Content-Range %q, want %q", got, want)
	}
	if sum(body) != sum(payload[from:to+1]) {
		t.Fatal("streamed bytes differ from the source")
	}
	if v, _ := findTask(m, id); v.State == StateDone {
		t.Fatal("the range only arrived once the whole file had: nothing was streamed")
	}

	waitFor(t, 30*time.Second, "completion", stateIs(m, id, StateDone))
	resp, err = http.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || sum(body) != sum(payload) {
		t.Fatalf("finished file: status %d, %d bytes", resp.StatusCode, len(body))
	}

	for path, want := range map[string]int{
		"/stream/nope/x.mkv?token=tok":       http.StatusNotFound,
		"/stream/" + id + "/film.mkv":        http.StatusUnauthorized,
		"/stream/" + id + "/film.mkv?token=": http.StatusUnauthorized,
	} {
		resp, err := http.Get(api.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s: status %d, want %d", path, resp.StatusCode, want)
		}
	}
}

func TestPlayOpensTheFileOrTheStream(t *testing.T) {
	var mu sync.Mutex
	var opened []string
	old := launchPlayer
	launchPlayer = func(target, title string) error {
		mu.Lock()
		opened = append(opened, target)
		mu.Unlock()
		return nil
	}
	defer func() { launchPlayer = old }()

	fast := httptest.NewServer(&rangedHandler{payload: makePayload(1 << 20)})
	defer fast.Close()
	slow := httptest.NewServer(&slowServer{payload: makePayload(8 << 20), chunk: 8 << 10, delay: 50 * time.Millisecond})
	defer slow.Close()
	m := NewManager(t.TempDir(), 2)
	s := &server{mgr: m, token: "tok", port: 16801}
	play := func(id string) map[string]any {
		rec := httptest.NewRecorder()
		s.handlePlay(rec, httptest.NewRequest(http.MethodPost, "/api/play?id="+id, nil))
		var out map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	finished, _ := m.Add(jobRequest{URL: fast.URL + "/clip.mp4"})
	waitFor(t, 10*time.Second, "completion", stateIs(m, finished, StateDone))
	if r := play(finished); r["ok"] != true {
		t.Fatalf("play a finished file: %v", r)
	}
	running, _ := m.Add(jobRequest{URL: slow.URL + "/film.mkv", Connections: 2})
	waitFor(t, 10*time.Second, "the second download to run", stateIs(m, running, StateRunning))
	if r := play(running); r["ok"] != true {
		t.Fatalf("play a download in progress: %v", r)
	}
	if r := play("nope"); r["ok"] != false {
		t.Fatalf("play an unknown task: %v", r)
	}
	m.Pause(running)

	v, _ := findTask(m, finished)
	mu.Lock()
	defer mu.Unlock()
	if len(opened) != 2 || opened[0] != v.Path {
		t.Fatalf("opened %q, want the finished file first", opened)
	}
	if want := "http://127.0.0.1:16801/stream/" + running + "/film.mkv?token=tok"; opened[1] != want {
		t.Errorf("stream link %q, want %q", opened[1], want)
	}
}

// Play must not open a player on a stream that can only refuse it. A stream
// playlist or a yt-dlp job that has not started looks like any other
// download, and one whose link expired has nothing more coming until the link
// is refreshed.
func TestPlayRefusesWhatCannotBeStreamed(t *testing.T) {
	var mu sync.Mutex
	var opened []string
	old := launchPlayer
	launchPlayer = func(target, title string) error {
		mu.Lock()
		opened = append(opened, target)
		mu.Unlock()
		return nil
	}
	defer func() { launchPlayer = old }()

	dir := t.TempDir()
	m := NewManager(dir, 1)
	m.announce = nil
	// Keep the downloads queued, so nothing here needs the network.
	m.mu.Lock()
	m.running = m.limit
	m.mu.Unlock()
	s := &server{mgr: m, token: "tok", port: 16801}
	play := func(id string) map[string]any {
		rec := httptest.NewRecorder()
		s.handlePlay(rec, httptest.NewRequest(http.MethodPost, "/api/play?id="+id, nil))
		var out map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	hls, _ := m.Add(jobRequest{URL: "https://example.invalid/show/index.m3u8"})
	defer m.Pause(hls)
	yt, _ := m.Add(jobRequest{URL: "https://example.invalid/watch?v=x", Kind: "yt-dlp"})
	defer m.Pause(yt)
	expired := &managedTask{
		view: TaskView{
			ID: "expired", URL: "https://example.invalid/film.mkv", Filename: "film.mkv",
			Path: filepath.Join(dir, "film.mkv"), Size: 1 << 20, State: StateNeedsRefresh,
		},
		req:    jobRequest{URL: "https://example.invalid/film.mkv"},
		outDir: dir,
	}
	m.mu.Lock()
	m.tasks["expired"] = expired
	m.order = append(m.order, "expired")
	m.mu.Unlock()

	for what, id := range map[string]string{"a queued stream playlist": hls, "a queued yt-dlp job": yt, "an expired link": "expired"} {
		r := play(id)
		if msg, _ := r["error"].(string); r["ok"] != false || msg == "" {
			t.Errorf("play %s: %v", what, r)
		}
	}
	if st := expired.snapshot().State; st != StateNeedsRefresh {
		t.Errorf("playing an expired link left it %s", st)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(opened) > 0 {
		t.Errorf("opened %q", opened)
	}
}
