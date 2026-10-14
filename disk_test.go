package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// diskOfItsOwn gives the test a disk of its own, which every folder it makes
// with t.TempDir is on, with free(dir) bytes free. A download another test
// left running is on another disk, so what it still needs does not count.
func diskOfItsOwn(t *testing.T, free func(dir string) int64) {
	t.Helper()
	root := filepath.Dir(t.TempDir())
	mine := func(d string) bool {
		rel, err := filepath.Rel(root, d)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	oldFree, oldDisk := freeSpace, diskOf
	freeSpace = func(d string) (int64, bool) {
		if mine(d) {
			return free(d), true
		}
		return oldFree(d)
	}
	diskOf = func(d string) string {
		if mine(d) {
			return "the disk of " + t.Name()
		}
		return oldDisk(d)
	}
	t.Cleanup(func() { freeSpace, diskOf = oldFree, oldDisk })
}

func needs(n int64) func() int64 { return func() int64 { return n } }

// Sparse files take their space only as they are written, so every download
// starting beside others that have written little would see the same free
// space. What the ones running on the disk still need counts as taken.
func TestTheClaimsOnADiskAddUp(t *testing.T) {
	var free atomic.Int64
	diskOfItsOwn(t, func(string) int64 { return free.Load() })
	a, b, c := t.TempDir(), t.TempDir(), t.TempDir()

	// Three downloads of 50 on a disk with 100 free: two fit.
	free.Store(100)
	releaseA, err := claimRoom(a, needs(50))
	if err != nil {
		t.Fatalf("the first 50 of 100 free: %v", err)
	}
	releaseB, err := claimRoom(b, needs(50))
	if err != nil {
		t.Fatalf("the second 50 of 100 free: %v", err)
	}
	_, err = claimRoom(c, needs(50))
	if err == nil || !strings.Contains(err.Error(), "not enough free space") ||
		!strings.Contains(err.Error(), "already running") {
		t.Fatalf("a third 50 beside two that still need 100 of 100 free: %v, want it refused for their room", err)
	}

	// One stopping, done or paused, lets its claim go.
	releaseA()
	releaseA()
	releaseC, err := claimRoom(c, needs(50))
	if err != nil {
		t.Fatalf("the third once the first has stopped: %v", err)
	}
	releaseC()

	// What a download still needs is asked each time, and can grow: an
	// ordinary file from an older godm takes all its space at first, and once
	// sparse gives back what it had not been written, which it still needs.
	var left atomic.Int64
	releaseB()
	free.Store(40)
	releaseOld, err := claimRoom(a, left.Load)
	if err != nil {
		t.Fatalf("a file that already takes all its space, on a disk with 40 free: %v", err)
	}
	defer releaseOld()
	left.Store(50)
	free.Store(90)
	if _, err := claimRoom(b, needs(60)); err == nil {
		t.Fatal("60 of 90 free was given beside a download that gave back 50 it still needs")
	}
	releaseD, err := claimRoom(b, needs(40))
	if err != nil {
		t.Fatalf("40 of what the other leaves of 90 free: %v", err)
	}
	releaseD()
}

// heldServer answers the probe at once and holds every other response after
// its first first bytes until hold is closed.
type heldServer struct {
	payload []byte
	first   int
	hold    chan struct{}
}

func (h *heldServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start, end := rangeOf(r, len(h.payload))
	body := h.payload[start : end+1]
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(h.payload)))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusPartialContent)
	if end > start {
		n := min(h.first, len(body))
		if _, err := w.Write(body[:n]); err != nil {
			return
		}
		body = body[n:]
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-h.hold:
		case <-r.Context().Done():
			return
		}
	}
	w.Write(body)
}

// startHeld starts a download from srv that its server holds back, and
// returns once it is under way, with the way to stop it and wait for it.
func startHeld(t *testing.T, o Options) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	o.OnStart = func(StartInfo) { close(started) }
	go func() {
		_, err := Download(ctx, o)
		done <- err
	}()
	select {
	case <-started:
	case err := <-done:
		cancel()
		t.Fatalf("the held download did not start: %v", err)
	case <-time.After(20 * time.Second):
		cancel()
		t.Fatal("the held download did not start")
	}
	var stopped bool
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Error("the held download did not stop")
		}
	}
	t.Cleanup(stop)
	return stop
}

// A download's file took its whole space when it was made, so the second of
// two that did not both fit was refused at once and the first finished.
// Sparse, the first has taken next to nothing when the second starts, and
// the room it still needs is what refuses the second.
func TestASecondDownloadIsRefusedTheRoomTheFirstStillNeeds(t *testing.T) {
	payload := makePayload(4 << 20)
	held := &heldServer{payload: payload, hold: make(chan struct{})}
	srvA := httptest.NewServer(held)
	defer srvA.Close()
	defer close(held.hold)
	srvB := httptest.NewServer(&slowServer{payload: payload, chunk: 256 << 10})
	defer srvB.Close()
	diskOfItsOwn(t, func(string) int64 { return 6 << 20 })

	stopA := startHeld(t, Options{URL: srvA.URL + "/a.mkv", OutDir: t.TempDir(), Connections: 2, MinSplit: 64 << 10})
	// Another folder on the same disk.
	b := Options{URL: srvB.URL + "/b.mkv", OutDir: t.TempDir(), Connections: 2, MinSplit: 64 << 10}
	_, err := Download(context.Background(), b)
	if err == nil || !strings.Contains(err.Error(), "not enough free space") {
		t.Fatalf("4 MB beside a download that still needs 4 MB of 6 MB free: %v, want it refused", err)
	}
	if names := dirNames(t, b.OutDir); len(names) != 0 {
		t.Errorf("the refused download left %v behind", names)
	}

	// Paused, the first holds no room.
	stopA()
	res, err := Download(context.Background(), b)
	if err != nil {
		t.Fatalf("4 MB of 6 MB free once the other download has stopped: %v", err)
	}
	checkFile(t, res.Path, payload)
}

// An earlier attempt at the same link that can no longer be continued is
// started over in place. Refused for want of room, the attempt must leave it
// as it was, so that the next one still starts it over there, rather than
// leave a partial file nothing knows of and move on to a numbered name.
func TestARefusedStartOverLeavesTheEarlierAttemptToTheNext(t *testing.T) {
	dir := t.TempDir()
	payload := makePayload(4 << 20)
	srv := httptest.NewServer(&slowServer{payload: payload, chunk: 256 << 10})
	defer srv.Close()
	url := srv.URL + "/film.mkv"
	path := filepath.Join(dir, "film.mkv")
	// The file on the server has changed size since.
	if err := os.WriteFile(path, payload[:1<<20], 0o644); err != nil {
		t.Fatal(err)
	}
	old := &State{Version: stateVersion, URL: url, FinalURL: url, Size: 3 << 20, Filename: "film.mkv",
		Segments: []SegSnap{{Start: 0, End: 3<<20 - 1, Done: 1 << 20}}}
	if err := saveState(path, old); err != nil {
		t.Fatal(err)
	}
	var free atomic.Int64
	diskOfItsOwn(t, func(string) int64 { return free.Load() })
	o := Options{URL: url, OutDir: dir, Connections: 2, MinSplit: 64 << 10}

	free.Store(2 << 20)
	_, err := Download(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "not enough free space") {
		t.Fatalf("4 MB over a 1 MB attempt with 2 MB free: %v, want it refused for want of space", err)
	}
	if _, ok := loadState(path); !ok {
		t.Error("the refused attempt dropped the sidecar of the one before it")
	}

	free.Store(4 << 20)
	res, err := Download(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != path {
		t.Errorf("the download went to %s, want it started over in %s", filepath.Base(res.Path), filepath.Base(path))
	}
	checkFile(t, res.Path, payload)
	if names := dirNames(t, dir); len(names) != 1 {
		t.Errorf("the folder holds %v, want only the download", names)
	}
}

// A torrent's files are sparse too, so it holds the room it still needs while
// it downloads, and lets it go when it stops.
func TestATorrentHoldsTheRoomItStillNeeds(t *testing.T) {
	src := t.TempDir()
	payload := makePayload(2 << 20)
	mi := makeTorrent(t, src, "film.bin", 64<<10, tfile{data: payload})
	// Slow enough to be in the middle of it for a while.
	seed := newSeeder(t, src, mi, 64<<10)
	m := newBTManager(t)
	diskOfItsOwn(t, func(string) int64 { return 3 << 20 })

	id, err := m.Add(jobRequest{URL: magnetOf(mi, "film.bin", seed.addr())})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "the torrent to start downloading", func() bool {
		v, _ := findTask(m, id)
		return v.Received > 0
	})

	srv := httptest.NewServer(&slowServer{payload: payload, chunk: 256 << 10})
	defer srv.Close()
	o := Options{URL: srv.URL + "/other.bin", OutDir: t.TempDir(), Connections: 2, MinSplit: 64 << 10}
	_, err = Download(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "not enough free space") {
		v, _ := findTask(m, id)
		t.Fatalf("2 MB beside a torrent with %d of 2 MB still to come, on 3 MB free: %v, want it refused",
			v.Size-v.Received, err)
	}

	if !m.Pause(id) {
		t.Fatal("the torrent could not be paused")
	}
	waitFor(t, 20*time.Second, "the torrent to pause", stateIs(m, id, StatePaused))
	// Its run may still be on its way out when the state shows.
	var res *Result
	waitFor(t, 10*time.Second, "the download to find room", func() bool {
		res, err = Download(context.Background(), o)
		if err != nil && !strings.Contains(err.Error(), "not enough free space") {
			t.Fatal(err)
		}
		return err == nil
	})
	checkFile(t, res.Path, payload)
}
