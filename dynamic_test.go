package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------- never overwrite ----------

func TestNeverOverwritesAFinishedFile(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "report.pdf")
	if err := os.WriteFile(existing, []byte("my precious file"), 0o644); err != nil {
		t.Fatal(err)
	}
	payload := makePayload(300 << 10)
	srv := httptest.NewServer(&rangedHandler{payload: payload, disp: `attachment; filename="report.pdf"`})
	defer srv.Close()

	for n, want := range []string{"report (1).pdf", "report (2).pdf"} {
		res, err := Download(context.Background(), Options{URL: srv.URL + "/x", OutDir: dir, Connections: 2, MinSplit: 4 << 10})
		if err != nil {
			t.Fatal(err)
		}
		if got := filepath.Base(res.Path); got != want {
			t.Errorf("download %d went to %q, want %q", n+1, got, want)
		}
		got, _ := os.ReadFile(res.Path)
		if sum(got) != sum(payload) {
			t.Errorf("download %d content mismatch", n+1)
		}
	}
	if got, _ := os.ReadFile(existing); string(got) != "my precious file" {
		t.Fatalf("the existing file was overwritten: now %d bytes", len(got))
	}
}

func TestSameNameDownloadsDoNotShareAFile(t *testing.T) {
	small, large := makePayload(1<<20), makePayload(3<<19)
	a := httptest.NewServer(&slowServer{payload: small, chunk: 32 << 10, delay: 5 * time.Millisecond})
	b := httptest.NewServer(&slowServer{payload: large, chunk: 32 << 10, delay: 5 * time.Millisecond})
	defer a.Close()
	defer b.Close()
	dir := t.TempDir()

	var wg sync.WaitGroup
	results := make([]*Result, 2)
	errs := make([]error, 2)
	for i, u := range []string{a.URL + "/same.bin", b.URL + "/same.bin"} {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			results[i], errs[i] = Download(context.Background(), Options{URL: u, OutDir: dir, Connections: 4, MinSplit: 32 << 10})
		}(i, u)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("download %d: %v", i, err)
		}
	}
	if results[0].Path == results[1].Path {
		t.Fatalf("two different files were written to the same path %s", results[0].Path)
	}
	for i, want := range [][]byte{small, large} {
		got, _ := os.ReadFile(results[i].Path)
		if sum(got) != sum(want) {
			t.Errorf("download %d (%s) was corrupted by the other", i, filepath.Base(results[i].Path))
		}
	}
}

func TestNumberedNames(t *testing.T) {
	cases := map[string]string{
		"movie.mkv":        "movie (3).mkv",
		"backup.tar.gz":    "backup (3).tar.gz",
		"README":           "README (3)",
		".bashrc":          ".bashrc (3)",
		"archive.TAR.XZ":   "archive (3).TAR.XZ",
		"v1.2.3-setup.exe": "v1.2.3-setup (3).exe",
	}
	for in, want := range cases {
		if got := filepath.Base(numberedName(filepath.Join("d", in), 3)); got != want {
			t.Errorf("numberedName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := numberedName(filepath.Join("d", "x.bin"), 0); filepath.Base(got) != "x.bin" {
		t.Errorf("n=0 should keep the name, got %q", got)
	}
}

// ---------- dynamic splitting ----------

// stragglerServer answers every range quickly except those starting at
// slowStart, which trickle: one connection landing on a bad route, exactly
// the case where a download crawls through its last segment.
type stragglerServer struct {
	payload   []byte
	slowStart int64
	requests  atomic.Int32
}

func (h *stragglerServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.requests.Add(1)
	start, end := rangeOf(r, len(h.payload))
	body := h.payload[start : end+1]
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(h.payload)))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusPartialContent)
	if start != h.slowStart {
		w.Write(body)
		return
	}
	f, _ := w.(http.Flusher)
	for len(body) > 0 {
		n := 8 << 10
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
		case <-time.After(250 * time.Millisecond): // 32 KiB/s
		}
	}
}

func TestSlowConnectionIsNotLeftAloneWithTheEnd(t *testing.T) {
	payload := makePayload(1 << 20) // four 256 KiB ranges
	h := &stragglerServer{payload: payload, slowStart: 512 << 10}
	srv := httptest.NewServer(h)
	defer srv.Close()

	start := time.Now()
	res, err := Download(context.Background(), Options{
		URL: srv.URL + "/f.bin", OutDir: t.TempDir(), Connections: 4, MinSplit: 16 << 10,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(res.Path)
	if sum(got) != sum(payload) {
		t.Fatal("content mismatch after splitting the slow range")
	}
	// On its own the slow connection needs 256 KiB / 32 KiB/s = 8s. The idle
	// connections should take most of its range away.
	t.Logf("finished in %s using %d segments and %d requests", elapsed.Round(10*time.Millisecond), res.Segments, h.requests.Load())
	if elapsed > 4*time.Second {
		t.Errorf("took %s: the slow connection's range was not shared out", elapsed)
	}
	if res.Segments <= 4 {
		t.Errorf("no range was split (%d segments)", res.Segments)
	}
}

func TestConnectionLimitChangesWhileRunning(t *testing.T) {
	payload := makePayload(8 << 20)
	srv := httptest.NewServer(&slowServer{payload: payload, chunk: 8 << 10, delay: 30 * time.Millisecond})
	defer srv.Close()

	var limit, active, maxActive atomic.Int32
	limit.Store(2)
	var received atomic.Int64
	done := make(chan error, 1)
	var res *Result
	go func() {
		var err error
		res, err = Download(context.Background(), Options{
			URL: srv.URL + "/big.bin", OutDir: t.TempDir(), Connections: 2, MinSplit: 64 << 10,
			ConnLimit: func() int { return int(limit.Load()) },
			OnProgress: func(p Progress) {
				active.Store(int32(p.Active))
				received.Store(p.Received)
				if int32(p.Active) > maxActive.Load() {
					maxActive.Store(int32(p.Active))
				}
			},
		})
		done <- err
	}()

	waitFor(t, 5*time.Second, "the download to start", func() bool { return received.Load() > 0 })
	time.Sleep(900 * time.Millisecond)
	if m := maxActive.Load(); m > 2 {
		t.Fatalf("%d connections ran with a limit of 2", m)
	}

	limit.Store(6)
	waitFor(t, 5*time.Second, "extra connections after raising the limit to 6", func() bool { return active.Load() >= 5 })

	limit.Store(1)
	waitFor(t, 5*time.Second, "connections to be released after lowering the limit to 1", func() bool { return active.Load() <= 1 })

	limit.Store(8)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(res.Path)
	if sum(got) != sum(payload) {
		t.Fatal("content mismatch after changing the connection limit mid-download")
	}
}

// jitterServer gives each request its own speed, so ranges finish out of
// order and get split at arbitrary moments.
type jitterServer struct {
	payload []byte
	n       atomic.Int32
}

func (h *jitterServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	k := h.n.Add(1)
	start, end := rangeOf(r, len(h.payload))
	body := h.payload[start : end+1]
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(h.payload)))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusPartialContent)
	delay := time.Duration(k%5) * time.Millisecond
	f, _ := w.(http.Flusher)
	for len(body) > 0 {
		n := 4 << 10
		if n > len(body) {
			n = len(body)
		}
		if _, err := w.Write(body[:n]); err != nil {
			return
		}
		body = body[n:]
		if delay > 0 {
			if f != nil {
				f.Flush()
			}
			time.Sleep(delay)
		}
	}
}

// Splits race with writes constantly here; every byte must still land exactly
// once and in the right place.
func TestHeavySplittingNeverCorruptsTheFile(t *testing.T) {
	payload := makePayload(2 << 20)
	for i := 0; i < 5; i++ {
		srv := httptest.NewServer(&jitterServer{payload: payload})
		dir := t.TempDir()
		res, err := Download(context.Background(), Options{
			URL: srv.URL + "/j.bin", OutDir: dir, Connections: 8, MinSplit: 4 << 10,
		})
		srv.Close()
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		got, _ := os.ReadFile(res.Path)
		if sum(got) != sum(payload) || res.Size != int64(len(payload)) {
			t.Fatalf("run %d: corrupted (%d bytes, reported %d) after %d segments", i, len(got), res.Size, res.Segments)
		}
		if _, err := os.Stat(statePath(res.Path)); !os.IsNotExist(err) {
			t.Fatalf("run %d: sidecar left behind", i)
		}
	}
}

func TestManagerChangesConnectionsOfARunningDownload(t *testing.T) {
	payload := makePayload(8 << 20)
	srv := httptest.NewServer(&slowServer{payload: payload, chunk: 8 << 10, delay: 30 * time.Millisecond})
	defer srv.Close()

	m := NewManager(t.TempDir(), 2)
	id, _ := m.Add(jobRequest{URL: srv.URL + "/m.bin", Connections: 2})
	waitFor(t, 10*time.Second, "the download to run", func() bool {
		v, _ := findTask(m, id)
		return v.State == StateRunning && v.Received > 0
	})
	if v, _ := findTask(m, id); v.Conns != 2 || v.Active > 2 {
		t.Fatalf("before the change: conns=%d active=%d", v.Conns, v.Active)
	}

	if n, err := m.SetConnections(id, 6); err != nil || n != 6 {
		t.Fatalf("SetConnections = %d, %v", n, err)
	}
	waitFor(t, 5*time.Second, "the running task to use more connections", func() bool {
		v, _ := findTask(m, id)
		return v.Conns == 6 && v.Active >= 4
	})
	waitFor(t, 30*time.Second, "completion", stateIs(m, id, StateDone))

	v, _ := findTask(m, id)
	got, _ := os.ReadFile(v.Path)
	if sum(got) != sum(payload) {
		t.Fatal("content mismatch")
	}
	if m.tasks[id].req.Connections != 6 {
		t.Error("the new connection count was not kept for the task")
	}
	if _, err := m.SetConnections("nope", 4); err == nil {
		t.Error("expected an error for an unknown task")
	}
}
