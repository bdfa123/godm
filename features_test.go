package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// servePartial writes the requested range of payload with proper 206 headers
// and returns the body it intends to send.
func rangeOf(r *http.Request, size int) (start, end int64) {
	end = int64(size) - 1
	spec := strings.TrimPrefix(r.Header.Get("Range"), "bytes=")
	if spec == "" {
		return 0, end
	}
	parts := strings.SplitN(spec, "-", 2)
	start, _ = strconv.ParseInt(parts[0], 10, 64)
	if len(parts) > 1 && parts[1] != "" {
		if e, err := strconv.ParseInt(parts[1], 10, 64); err == nil && e < end {
			end = e
		}
	}
	return start, end
}

func hangUp(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			conn.Close()
		}
	}
}

// expiringServer serves one file under several link tokens. The "old" token
// dies after cutAfter bytes, the way a signed link lapses mid-download; any
// token listed in other serves a different file.
type expiringServer struct {
	payload  []byte
	cutAfter int64
	trickle  time.Duration // if set, bodies stream in 16 KiB chunks with this pause

	mu    sync.Mutex
	sent  map[string]int64
	dead  map[string]bool
	other map[string][]byte
	log   []string
}

func (h *expiringServer) requests() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.log, "\n")
}

func newExpiringServer(payload []byte, cutAfter int64) *expiringServer {
	return &expiringServer{
		payload: payload, cutAfter: cutAfter,
		sent: map[string]int64{}, dead: map[string]bool{}, other: map[string][]byte{},
	}
}

func (h *expiringServer) served(tok string) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sent[tok]
}

func (h *expiringServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("t")
	h.mu.Lock()
	payload := h.payload
	if p, ok := h.other[tok]; ok {
		payload = p
	}
	if h.dead[tok] {
		h.mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
		return
	}
	start, end := rangeOf(r, len(payload))
	body := payload[start : end+1]
	cut := false
	if tok == "old" {
		budget := h.cutAfter - h.sent[tok]
		if budget < 0 {
			budget = 0
		}
		if int64(len(body)) > budget {
			body, cut = body[:budget], true
			h.dead[tok] = true
		}
	}
	h.sent[tok] += int64(len(body))
	h.log = append(h.log, fmt.Sprintf("%-5s bytes=%d-%d sent=%d cut=%v", tok, start, end, len(body), cut))
	h.mu.Unlock()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="movie.bin"`)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(http.StatusPartialContent)
	if h.trickle <= 0 {
		w.Write(body)
	} else {
		f, _ := w.(http.Flusher)
		for len(body) > 0 {
			n := 16 << 10
			if n > len(body) {
				n = len(body)
			}
			if _, err := w.Write(body[:n]); err != nil {
				return
			}
			body = body[n:]
			if f != nil {
				f.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(h.trickle):
			}
		}
	}
	if cut {
		hangUp(w)
	}
}

// slowServer trickles bytes so a test can observe a download while it runs.
type slowServer struct {
	payload []byte
	chunk   int
	delay   time.Duration

	mu   sync.Mutex
	sent int64
}

func (h *slowServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start, end := rangeOf(r, len(h.payload))
	body := h.payload[start : end+1]
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(h.payload)))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusPartialContent)
	f, _ := w.(http.Flusher)
	for len(body) > 0 {
		n := h.chunk
		if n > len(body) {
			n = len(body)
		}
		if _, err := w.Write(body[:n]); err != nil {
			return
		}
		h.mu.Lock()
		h.sent += int64(n)
		h.mu.Unlock()
		body = body[n:]
		if f != nil {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(h.delay):
		}
	}
}

func (h *slowServer) total() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sent
}

// ---------- engine ----------

func TestProgressReportsLiveConnections(t *testing.T) {
	// Slow enough to span several 400ms progress ticks.
	payload := makePayload(2 << 20)
	srv := httptest.NewServer(&slowServer{payload: payload, chunk: 16 << 10, delay: 40 * time.Millisecond})
	defer srv.Close()

	var mu sync.Mutex
	maxActive, segCount := 0, 0
	var last Progress
	res, err := Download(context.Background(), Options{
		URL: srv.URL + "/live.bin", OutDir: t.TempDir(), Connections: 4, MinSplit: 64 << 10,
		OnProgress: func(p Progress) {
			mu.Lock()
			defer mu.Unlock()
			if p.Active > maxActive {
				maxActive = p.Active
			}
			segCount = len(p.Segments)
			last = p
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if segCount != 4 || res.Segments != 4 {
		t.Fatalf("expected 4 segments, progress saw %d, result %d", segCount, res.Segments)
	}
	if maxActive < 2 {
		t.Errorf("never saw more than %d connection active at once", maxActive)
	}
	for i, s := range last.Segments {
		if s.State != "done" {
			t.Errorf("segment %d ended as %q, want done", i, s.State)
		}
	}
}

func TestExpiredLinkKeepsProgressAndRefreshContinues(t *testing.T) {
	payload := makePayload(2 << 20)
	h := newExpiringServer(payload, 700<<10)
	srv := httptest.NewServer(h)
	defer srv.Close()
	dir := t.TempDir()

	_, err := Download(context.Background(), Options{
		URL: srv.URL + "/f?t=old", OutDir: dir, Connections: 4, MinSplit: 4 << 10, MaxRetries: 3,
	})
	var expired *LinkExpiredError
	if !errors.As(err, &expired) {
		t.Fatalf("want LinkExpiredError once the link dies, got %v", err)
	}

	target := filepath.Join(dir, "movie.bin")
	st, ok := loadState(target)
	if !ok {
		t.Fatal("the sidecar must survive an expired link")
	}
	var kept int64
	for _, s := range st.Segments {
		kept += s.Done
	}
	if kept == 0 || kept >= int64(len(payload)) {
		t.Fatalf("expected partial progress before expiry, kept %d", kept)
	}

	res, err := Download(context.Background(), Options{
		URL: srv.URL + "/f?t=new", OutDir: dir, Filename: "movie.bin",
		Connections: 4, MinSplit: 4 << 10, Refresh: true,
	})
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if !res.Resumed {
		t.Fatal("a refresh must continue the partial file, not start over")
	}
	got, _ := os.ReadFile(res.Path)
	if sum(got) != sum(payload) {
		t.Fatal("file assembled from two links does not match the source")
	}
	// Only the missing bytes (plus the one-byte probe) may come from the new link.
	if n := h.served("new"); n > int64(len(payload))-kept+1 {
		t.Errorf("new link served %d bytes, but only %d were missing", n, int64(len(payload))-kept)
	}
}

// When a link dies, connections already receiving a valid response should
// finish their range instead of being cut, so the refresh has less to fetch.
func TestLinkExpiryLetsInFlightConnectionsFinish(t *testing.T) {
	payload := makePayload(2 << 20) // four 512 KiB segments
	h := newExpiringServer(payload, 700<<10)
	h.trickle = 15 * time.Millisecond // a full segment takes ~0.5s, the cut ones fail sooner
	srv := httptest.NewServer(h)
	defer srv.Close()
	dir := t.TempDir()

	_, err := Download(context.Background(), Options{
		URL: srv.URL + "/f?t=old", OutDir: dir, Connections: 4, MinSplit: 4 << 10, MaxRetries: 3,
	})
	var expired *LinkExpiredError
	if !errors.As(err, &expired) {
		t.Fatalf("want LinkExpiredError, got %v", err)
	}
	// Report the cause, not a connection that merely stopped because of it.
	if expired.consequence || !strings.Contains(err.Error(), "403") {
		t.Errorf("reported %q; want the 403 that killed the link", err)
	}
	st, ok := loadState(filepath.Join(dir, "movie.bin"))
	if !ok {
		t.Fatal("no sidecar after expiry")
	}
	var kept int64
	full := 0
	for _, s := range st.Segments {
		kept += s.Done
		if s.Done == s.End-s.Start+1 {
			full++
		}
	}
	// The first range request fit inside the link's remaining budget, so its
	// segment must have been allowed to complete.
	if full == 0 {
		t.Fatalf("no segment completed; in-flight transfers were cut when the link died (kept %d bytes)\n%s",
			kept, h.requests())
	}
}

func TestRefreshRefusesADifferentFile(t *testing.T) {
	payload := makePayload(1 << 20)
	h := newExpiringServer(payload, 300<<10)
	h.other["wrong"] = makePayload(3 << 20)
	srv := httptest.NewServer(h)
	defer srv.Close()
	dir := t.TempDir()

	Download(context.Background(), Options{
		URL: srv.URL + "/f?t=old", OutDir: dir, Connections: 4, MinSplit: 4 << 10, MaxRetries: 2,
	})
	target := filepath.Join(dir, "movie.bin")
	before, _ := os.ReadFile(target)

	_, err := Download(context.Background(), Options{
		URL: srv.URL + "/f?t=wrong", OutDir: dir, Filename: "movie.bin",
		Connections: 4, MinSplit: 4 << 10, Refresh: true,
	})
	var mismatch *RefreshMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("want RefreshMismatchError for a different file, got %v", err)
	}
	after, _ := os.ReadFile(target)
	if sum(before) != sum(after) {
		t.Fatal("a refused refresh must leave the partial file untouched")
	}
	if _, ok := loadState(target); !ok {
		t.Fatal("a refused refresh must keep the saved progress")
	}
}

func TestWebPageInsteadOfFileIsAnExpiredLink(t *testing.T) {
	const size = 512 << 10
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "bytes=0-0" {
			w.Header().Set("Content-Type", "application/zip")
			w.Header().Set("Content-Range", "bytes 0-0/"+strconv.Itoa(size))
			w.WriteHeader(http.StatusPartialContent)
			w.Write([]byte{0})
			return
		}
		// Session expired: every real request bounces to a login page.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("<html><body>Please sign in</body></html>"))
	}))
	defer srv.Close()

	_, err := Download(context.Background(), Options{
		URL: srv.URL + "/a.zip", OutDir: t.TempDir(), Connections: 2, MinSplit: 4 << 10,
	})
	var expired *LinkExpiredError
	if !errors.As(err, &expired) {
		t.Fatalf("an HTML login page in place of a zip should read as an expired link, got %v", err)
	}
}

func TestThrottlingPageIsRetriedNotTreatedAsExpired(t *testing.T) {
	payload := makePayload(256 << 10)
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		n := hits
		mu.Unlock()
		if n == 2 { // first real segment request: a CDN "slow down" page
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte("<html>rate limited</html>"))
			return
		}
		start, end := rangeOf(r, len(payload))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(payload[start : end+1])
	}))
	defer srv.Close()

	res, err := Download(context.Background(), Options{
		URL: srv.URL + "/t.bin", OutDir: t.TempDir(), Connections: 1, MinSplit: 1 << 30,
	})
	if err != nil {
		t.Fatalf("an HTML 429 page must be retried, got %v", err)
	}
	got, _ := os.ReadFile(res.Path)
	if sum(got) != sum(payload) {
		t.Fatal("content mismatch after retrying a throttled request")
	}
}

// ---------- manager ----------

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func findTask(m *Manager, id string) (TaskView, bool) {
	for _, v := range m.List() {
		if v.ID == id {
			return v, true
		}
	}
	return TaskView{}, false
}

func stateIs(m *Manager, id string, st TaskState) func() bool {
	return func() bool {
		v, ok := findTask(m, id)
		return ok && v.State == st
	}
}

func TestManagerAdoptsRefreshedLink(t *testing.T) {
	payload := makePayload(2 << 20)
	h := newExpiringServer(payload, 700<<10)
	srv := httptest.NewServer(h)
	defer srv.Close()
	dir := t.TempDir()

	m := NewManager(dir, 2)
	opened := make(chan string, 1)
	m.openURL = func(u string) error { opened <- u; return nil }

	const page = "https://files.example/share/abc"
	id, err := m.Add(jobRequest{URL: srv.URL + "/f?t=old", Referrer: page, Connections: 4})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the link to expire", stateIs(m, id, StateNeedsRefresh))
	expiredView, _ := findTask(m, id)
	var kept int64
	if st, ok := loadState(expiredView.Path); ok {
		for _, s := range st.Segments {
			kept += s.Done
		}
	}

	ref, err := m.RequestRefresh(id)
	if err != nil || ref != page {
		t.Fatalf("RequestRefresh = %q, %v", ref, err)
	}
	select {
	case u := <-opened:
		if u != page {
			t.Fatalf("opened %q, want the source page", u)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the source page was not opened")
	}
	if v, _ := findTask(m, id); v.State != StateAwaitingRefresh {
		t.Fatalf("state = %s, want awaiting_refresh", v.State)
	}

	// The user clicks the link again; the extension hands godm a brand new job.
	newID, err := m.Add(jobRequest{URL: srv.URL + "/f?t=new", Referrer: page, Connections: 4})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the original task to finish", stateIs(m, id, StateDone))

	if _, still := findTask(m, newID); still {
		t.Fatal("the new job should have been absorbed into the waiting task")
	}
	v, _ := findTask(m, id)
	if !strings.Contains(v.URL, "t=new") {
		t.Errorf("task URL = %s, want the refreshed link", v.URL)
	}
	got, _ := os.ReadFile(v.Path)
	if sum(got) != sum(payload) {
		t.Fatal("refreshed download does not match the source")
	}
	// Two one-byte probes (the adoption check and the engine's own) plus the
	// bytes that were still missing. Nothing already on disk may be refetched.
	if missing := int64(len(payload)) - kept; h.served("new") > missing+2 {
		t.Errorf("new link served %d bytes but only %d were missing; requests:\n%s",
			h.served("new"), missing, h.requests())
	}
}

func TestManagerDoesNotAdoptADifferentSizedDownload(t *testing.T) {
	payload := makePayload(1 << 20)
	h := newExpiringServer(payload, 300<<10)
	h.other["unrelated"] = makePayload(600 << 10)
	srv := httptest.NewServer(h)
	defer srv.Close()

	m := NewManager(t.TempDir(), 2)
	m.openURL = func(string) error { return nil }
	id, _ := m.Add(jobRequest{URL: srv.URL + "/f?t=old", Referrer: "https://x.example/", Connections: 4})
	waitFor(t, 20*time.Second, "expiry", stateIs(m, id, StateNeedsRefresh))
	if _, err := m.RequestRefresh(id); err != nil {
		t.Fatal(err)
	}

	otherID, _ := m.Add(jobRequest{URL: srv.URL + "/other.bin?t=unrelated"})
	waitFor(t, 20*time.Second, "the unrelated download to finish on its own", stateIs(m, otherID, StateDone))
	if v, _ := findTask(m, id); v.State != StateAwaitingRefresh {
		t.Fatalf("waiting task changed to %s after an unrelated download", v.State)
	}
}

func TestQueueRunsInOrderWithinLimit(t *testing.T) {
	payload := makePayload(160 << 10)
	srv := httptest.NewServer(&slowServer{payload: payload, chunk: 16 << 10, delay: 20 * time.Millisecond})
	defer srv.Close()

	m := NewManager(t.TempDir(), 2)
	var ids []string
	for i := 1; i <= 5; i++ {
		id, err := m.Add(jobRequest{URL: fmt.Sprintf("%s/f%d.bin", srv.URL, i), Connections: 1})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	maxRunning := 0
	waitFor(t, 30*time.Second, "all downloads", func() bool {
		running, done := 0, 0
		for _, v := range m.List() {
			switch v.State {
			case StateRunning:
				running++
			case StateDone:
				done++
			}
		}
		if running > maxRunning {
			maxRunning = running
		}
		return done == len(ids)
	})
	if maxRunning > 2 {
		t.Fatalf("%d downloads ran at once with a limit of 2", maxRunning)
	}
	if maxRunning < 2 {
		t.Errorf("never used both slots (max %d running)", maxRunning)
	}

	var prev time.Time
	for i, id := range ids {
		v, _ := findTask(m, id)
		if v.StartedAt.Before(prev) {
			t.Fatalf("task %d started before the task queued ahead of it", i+1)
		}
		prev = v.StartedAt
	}
}

func TestPauseKeepsProgressAndResumeContinues(t *testing.T) {
	// Roughly 1.3s per segment, so the pause lands mid-transfer.
	payload := makePayload(1 << 20)
	h := &slowServer{payload: payload, chunk: 8 << 10, delay: 40 * time.Millisecond}
	srv := httptest.NewServer(h)
	defer srv.Close()

	m := NewManager(t.TempDir(), 2)
	id, _ := m.Add(jobRequest{URL: srv.URL + "/p.bin", Connections: 4})
	waitFor(t, 10*time.Second, "some progress", func() bool {
		v, _ := findTask(m, id)
		return v.Received > 64<<10
	})

	m.Pause(id)
	waitFor(t, 10*time.Second, "pause", stateIs(m, id, StatePaused))
	v, _ := findTask(m, id)
	if _, ok := loadState(v.Path); !ok {
		t.Fatal("pausing must leave the sidecar for resume")
	}
	if !m.Resume(id) {
		t.Fatal("resume refused")
	}
	waitFor(t, 20*time.Second, "completion", stateIs(m, id, StateDone))

	v, _ = findTask(m, id)
	got, _ := os.ReadFile(v.Path)
	if sum(got) != sum(payload) {
		t.Fatal("paused and resumed file does not match")
	}
	if h.total() > int64(len(payload))*3/2 {
		t.Errorf("served %d bytes for a %d byte file; resume restarted instead of continuing", h.total(), len(payload))
	}
}

func TestRemoveWithDeleteRemovesPartialFile(t *testing.T) {
	payload := makePayload(1 << 20)
	srv := httptest.NewServer(&slowServer{payload: payload, chunk: 8 << 10, delay: 20 * time.Millisecond})
	defer srv.Close()

	m := NewManager(t.TempDir(), 2)
	id, _ := m.Add(jobRequest{URL: srv.URL + "/big.bin", Connections: 4})
	var path string
	waitFor(t, 10*time.Second, "the file to be created", func() bool {
		v, _ := findTask(m, id)
		path = v.Path
		return v.Received > 0 && path != ""
	})

	m.Remove(id, true)
	waitFor(t, 10*time.Second, "the partial file to be deleted", func() bool {
		_, err1 := os.Stat(path)
		_, err2 := os.Stat(statePath(path))
		return os.IsNotExist(err1) && os.IsNotExist(err2)
	})
	if _, ok := findTask(m, id); ok {
		t.Fatal("removed task is still listed")
	}
}

func TestTaskListSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "tasks.json")

	m := NewManager(dir, 4)
	m.store = store
	add := func(id string, st TaskState) {
		m.tasks[id] = &managedTask{
			view:   TaskView{ID: id, URL: "https://h.example/" + id, State: st, AddedAt: time.Now()},
			req:    jobRequest{URL: "https://h.example/" + id, Headers: map[string]string{"Cookie": "s=1"}},
			outDir: dir,
		}
		m.order = append(m.order, id)
	}
	add("running", StateRunning)
	add("queued", StateQueued)
	add("done", StateDone)
	add("waiting", StateAwaitingRefresh)
	add("expired", StateNeedsRefresh)
	m.SetLimit(5)
	if err := m.save(); err != nil {
		t.Fatal(err)
	}

	m2 := NewManager(dir, 1)
	m2.store = store
	if err := m2.load(); err != nil {
		t.Fatal(err)
	}
	want := map[string]TaskState{
		"running": StatePaused, // never restart large downloads behind the user's back
		"queued":  StatePaused,
		"done":    StateDone,
		"waiting": StateNeedsRefresh,
		"expired": StateNeedsRefresh,
	}
	for id, st := range want {
		mt := m2.tasks[id]
		if mt == nil {
			t.Fatalf("task %s was not restored", id)
		}
		if mt.view.State != st {
			t.Errorf("%s restored as %s, want %s", id, mt.view.State, st)
		}
		if mt.req.Headers["Cookie"] != "s=1" {
			t.Errorf("%s lost its request headers", id)
		}
	}
	if m2.Limit() != 5 {
		t.Errorf("limit restored as %d, want 5", m2.Limit())
	}
	if len(m2.order) != 5 || m2.order[0] != "running" {
		t.Errorf("queue order not preserved: %v", m2.order)
	}
}
