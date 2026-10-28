package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A download that gets a new address keeps what it has saved, whichever way
// the address arrived and however long the run that first uses it takes to
// get going. These tests give a paused download with progress a second link
// to the same bytes and then keep that first run from reaching the file: it
// waits in the queue and is paused there, its first request is turned away,
// or godm is restarted while it waits.

// twoLinks serves one file slowly under any path, the way a signed link serves
// the same bytes under a new address each time it is clicked. It can turn the
// "/b/" links away for a while, or answer them with another file.
type twoLinks struct {
	file   *slowServer
	other  *slowServer // what "/b/" serves when it is not the same file
	refuse atomic.Bool
	wrong  atomic.Bool

	// When probing is set, the first request that asks "/b/" what it is says
	// so there and is not answered until letGo is closed, which lets a test
	// act while a link is being looked at.
	probing, letGo chan struct{}
	held           atomic.Bool
}

func (h *twoLinks) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/b/") {
		if h.probing != nil && r.Header.Get("Range") == "bytes=0-0" && h.held.CompareAndSwap(false, true) {
			close(h.probing)
			select {
			case <-h.letGo:
			case <-r.Context().Done():
				return
			}
		}
		if h.refuse.Load() {
			http.Error(w, "not now", http.StatusBadRequest)
			return
		}
		if h.wrong.Load() {
			h.other.ServeHTTP(w, r)
			return
		}
	}
	h.file.ServeHTTP(w, r)
}

func newTwoLinks(t *testing.T) (*twoLinks, *httptest.Server) {
	t.Helper()
	h := &twoLinks{
		// About a megabyte a second over two connections: slow enough to be
		// paused part way, quick enough to finish.
		file:  &slowServer{payload: makePayload(2 << 20), chunk: 16 << 10, delay: 30 * time.Millisecond},
		other: &slowServer{payload: makePayload(3 << 20), chunk: 64 << 10, delay: time.Millisecond},
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return h, srv
}

// pausedPartway downloads some of film.bin from the "/a/" link and pauses it.
func pausedPartway(t *testing.T, m *Manager, src string) (string, TaskView) {
	t.Helper()
	id, err := m.Add(jobRequest{URL: src + "/a/film.bin", Connections: 2})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "some of the file to arrive", func() bool {
		v, _ := findTask(m, id)
		return v.Received > 128<<10
	})
	m.Pause(id)
	waitFor(t, 10*time.Second, "the pause", stateIs(m, id, StatePaused))
	v, _ := findTask(m, id)
	if _, ok := loadState(v.Path); !ok || v.Received >= v.Size {
		t.Fatalf("paused with %d of %d bytes and no sidecar to continue from", v.Received, v.Size)
	}
	return id, v
}

// holdTheQueue fills the manager's one slot with a download that will not
// finish during the test, saved somewhere else so the folder under test holds
// only the file in question.
func holdTheQueue(t *testing.T, m *Manager) string {
	t.Helper()
	slow := httptest.NewServer(&slowServer{payload: makePayload(8 << 20), chunk: 8 << 10, delay: 50 * time.Millisecond})
	t.Cleanup(slow.Close)
	id, err := m.Add(jobRequest{URL: slow.URL + "/blocker.bin", Connections: 1, OutDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the other download to take the slot", stateIs(m, id, StateRunning))
	return id
}

func namesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// continued checks that the download finished in the file it had been paused
// in, and that nothing else was left beside it.
func continued(t *testing.T, m *Manager, id string, before TaskView, h *twoLinks, dir string) {
	t.Helper()
	waitFor(t, 30*time.Second, "the download to finish", stateIs(m, id, StateDone))
	v, _ := findTask(m, id)
	if v.Path != before.Path {
		t.Errorf("it had %d of %d bytes in %s and finished in %s instead: the download started over",
			before.Received, before.Size, filepath.Base(before.Path), filepath.Base(v.Path))
	}
	if got := namesIn(t, dir); len(got) != 1 || got[0] != "film.bin" {
		t.Errorf("the folder holds %v, want film.bin alone", got)
	}
	got, err := os.ReadFile(before.Path)
	if err != nil {
		t.Fatal(err)
	}
	if sum(got) != sum(h.file.payload) {
		t.Error("the file put together from two links does not match the source")
	}
	if !strings.Contains(v.URL, "/b/") {
		t.Errorf("the task's address is %s, want the new one", v.URL)
	}
}

func TestANewAddressStillContinuesAfterWaitingInTheQueue(t *testing.T) {
	h, src := newTwoLinks(t)
	dir := t.TempDir()
	m := NewManager(dir, 1)
	m.announce = nil
	id, before := pausedPartway(t, m, src.URL)
	blocker := holdTheQueue(t, m)

	if err := m.ChangeAddress(id, src.URL+"/b/film.bin"); err != nil {
		t.Fatal(err)
	}
	// It waits behind the other download, and is paused before its turn.
	m.Pause(id)
	m.Pause(blocker)
	waitFor(t, 10*time.Second, "the other download to stop", stateIs(m, blocker, StatePaused))
	if !m.Resume(id) {
		t.Fatal("resume refused")
	}
	continued(t, m, id, before, h, dir)
}

func TestANewAddressStillContinuesAfterItsFirstTryFails(t *testing.T) {
	h, src := newTwoLinks(t)
	dir := t.TempDir()
	m := NewManager(dir, 1)
	m.announce = nil
	id, before := pausedPartway(t, m, src.URL)

	h.refuse.Store(true)
	if err := m.ChangeAddress(id, src.URL+"/b/film.bin"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the first try to fail", stateIs(m, id, StateError))
	h.refuse.Store(false)
	if !m.Resume(id) {
		t.Fatal("retry refused")
	}
	continued(t, m, id, before, h, dir)
}

func TestANewAddressStillContinuesAfterARestart(t *testing.T) {
	h, src := newTwoLinks(t)
	dir := t.TempDir()
	store := filepath.Join(t.TempDir(), "tasks.json")
	m := NewManager(dir, 1)
	m.announce = nil
	m.store = store
	id, before := pausedPartway(t, m, src.URL)
	blocker := holdTheQueue(t, m)

	if err := m.ChangeAddress(id, src.URL+"/b/film.bin"); err != nil {
		t.Fatal(err)
	}
	// Quitting pauses everything and saves the list; a download that was
	// waiting comes back paused.
	m.PauseAll()
	waitFor(t, 10*time.Second, "everything to stop", func() bool {
		return stateIs(m, id, StatePaused)() && stateIs(m, blocker, StatePaused)()
	})
	if err := m.save(); err != nil {
		t.Fatal(err)
	}

	m2 := NewManager(dir, 1)
	m2.announce = nil
	m2.store = store
	if err := m2.load(); err != nil {
		t.Fatal(err)
	}
	if !m2.Resume(id) {
		t.Fatal("resume refused after the restart")
	}
	continued(t, m2, id, before, h, dir)
}

// The other way a download gets a new address: the user clicks the link again
// in the browser and the new download is matched to the one that waits for it.
func TestARefreshedLinkStillContinuesAfterWaitingInTheQueue(t *testing.T) {
	h, src := newTwoLinks(t)
	dir := t.TempDir()
	m := NewManager(dir, 1)
	m.announce = nil
	m.openURL = func(string) error { return nil }
	id, before := pausedPartway(t, m, src.URL)
	blocker := holdTheQueue(t, m)

	if _, err := m.RequestRefresh(id); err != nil {
		t.Fatal(err)
	}
	fresh, err := m.Add(jobRequest{URL: src.URL + "/b/film.bin", Connections: 2})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the new download to be taken for the fresh link", func() bool {
		_, listed := findTask(m, fresh)
		return !listed && stateIs(m, id, StateQueued)()
	})
	m.Pause(id)
	m.Pause(blocker)
	waitFor(t, 10*time.Second, "the other download to stop", stateIs(m, blocker, StatePaused))
	if !m.Resume(id) {
		t.Fatal("resume refused")
	}
	continued(t, m, id, before, h, dir)
}

// The promise that goes with it holds as long: an address that turns out to be
// another file is refused, not downloaded beside the partial one, however late
// the run that finds out.
func TestAWrongAddressIsStillRefusedAfterWaitingInTheQueue(t *testing.T) {
	h, src := newTwoLinks(t)
	h.wrong.Store(true)
	dir := t.TempDir()
	m := NewManager(dir, 1)
	m.announce = nil
	id, before := pausedPartway(t, m, src.URL)
	blocker := holdTheQueue(t, m)
	partial, err := os.ReadFile(before.Path)
	if err != nil {
		t.Fatal(err)
	}

	if err := m.ChangeAddress(id, src.URL+"/b/film.bin"); err != nil {
		t.Fatal(err)
	}
	m.Pause(id)
	m.Pause(blocker)
	waitFor(t, 10*time.Second, "the other download to stop", stateIs(m, blocker, StatePaused))
	if !m.Resume(id) {
		t.Fatal("resume refused")
	}
	waitFor(t, 30*time.Second, "the download to stop one way or another", func() bool {
		v, _ := findTask(m, id)
		return v.State == StateNeedsRefresh || v.State == StateDone || v.State == StateError
	})
	v, _ := findTask(m, id)
	if v.State != StateNeedsRefresh || !strings.Contains(v.Error, "different file") {
		t.Errorf("ended %s (%q) in %s, want the address refused as another file",
			v.State, v.Error, filepath.Base(v.Path))
	}
	if got := namesIn(t, dir); len(got) != 2 || got[0] != "film.bin" || got[1] != "film.bin.godm" {
		t.Errorf("the folder holds %v, want the partial file and its sidecar and nothing else", got)
	}
	if after, _ := os.ReadFile(before.Path); sum(after) != sum(partial) {
		t.Error("the partial file was written to")
	}

	// Nor is it tried again by itself, which would fetch the other file
	// afresh beside this one.
	if !v.RetryAt.IsZero() {
		t.Errorf("an address leading to another file is to be tried again at %v", v.RetryAt)
	}
	m.retryTick(time.Now().Add(time.Hour))
	time.Sleep(300 * time.Millisecond)
	if got := namesIn(t, dir); len(got) != 2 {
		t.Errorf("after the time for a try again the folder holds %v", got)
	}
}

// Matching a new download to the one that waits for its link takes a request
// to the new link, and the user can do anything in that time. If what they do
// is remove the waiting download, the new one is all there is: it must go on
// as a download of its own, not be handed to a task that is no longer there.
func TestRemovingTheWaitingDownloadWhileItsLinkIsLookedAtKeepsTheNewOne(t *testing.T) {
	for _, c := range []struct {
		name       string
		deleteFile bool
	}{
		{"removed from the list", false},
		{"removed with its file", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, src := newTwoLinks(t)
			h.probing, h.letGo = make(chan struct{}), make(chan struct{})
			dir := t.TempDir()
			m := NewManager(dir, 2)
			m.announce = nil
			m.openURL = func(string) error { return nil }
			old, _ := pausedPartway(t, m, src.URL)
			if _, err := m.RequestRefresh(old); err != nil {
				t.Fatal(err)
			}

			fresh, err := m.Add(jobRequest{URL: src.URL + "/b/film.bin", Connections: 2})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-h.probing:
			case <-time.After(20 * time.Second):
				t.Fatal("the new link was never looked at")
			}
			if !m.Remove(old, c.deleteFile) {
				t.Fatal("the waiting download could not be removed")
			}
			close(h.letGo)

			waitFor(t, 30*time.Second, "the new download to finish or be lost", func() bool {
				v, listed := findTask(m, fresh)
				return !listed || v.State == StateDone
			})
			v, listed := findTask(m, fresh)
			if !listed {
				t.Fatalf("the new download vanished from the list along with the one it was matched to; "+
					"the list is now %v and nothing is downloading", m.List())
			}
			if got, _ := os.ReadFile(v.Path); sum(got) != sum(h.file.payload) {
				t.Errorf("%s does not match the source", filepath.Base(v.Path))
			}
			if _, listed := findTask(m, old); listed {
				t.Error("the removed download is back in the list")
			}
		})
	}
}

// ---------- a stream ----------

// A stream is offered Change address like any other download, and its playlist
// is as likely to be a signed link. What it has saved is segments, counted in
// a sidecar of its own kind.
func pausedStream(t *testing.T, m *Manager, s *hlsServer) (string, TaskView) {
	t.Helper()
	// The first three arrive at once and the rest take their time, so the
	// pause lands with some segments written and most still to come.
	s.mu.Lock()
	for i := 3; i < len(s.segs); i++ {
		s.delay[i] = 300 * time.Millisecond
	}
	s.mu.Unlock()
	id, err := m.Add(jobRequest{URL: s.URL + "/show.m3u8?sig=old", Connections: 1, Variant: -1})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the first segments", func() bool {
		v, _ := findTask(m, id)
		return v.SegDone >= 3
	})
	m.Pause(id)
	waitFor(t, 10*time.Second, "the pause", stateIs(m, id, StatePaused))
	v, _ := findTask(m, id)
	if st, ok := loadHLSState(v.Path); !ok || st.Next < 3 || st.Next >= len(s.segs) {
		t.Fatalf("paused with no sidecar to continue from, or nothing left to fetch: %+v", st)
	}
	// From here on every segment is quick. The map is replaced, not emptied:
	// requests still being served hold the old one.
	s.mu.Lock()
	s.delay = map[int]time.Duration{}
	s.mu.Unlock()
	return id, v
}

func TestANewAddressContinuesAStream(t *testing.T) {
	segs := makeSegments(12)
	s := newHLSServer(t, segs)
	dir := t.TempDir()
	m := NewManager(dir, 1)
	m.announce = nil
	id, before := pausedStream(t, m, s)

	// The same playlist with a new signature, which is what a fresh link is.
	if err := m.ChangeAddress(id, s.URL+"/show.m3u8?sig=new"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "the stream to finish", stateIs(m, id, StateDone))
	v, _ := findTask(m, id)
	if v.Path != before.Path {
		t.Errorf("with %d of %d segments saved in %s the new address started over in %s",
			before.SegDone, before.SegTotal, filepath.Base(before.Path), filepath.Base(v.Path))
	}
	if got := namesIn(t, dir); len(got) != 1 || got[0] != "show.ts" {
		t.Errorf("the folder holds %v, want show.ts alone", got)
	}
	if n := s.hitCount("/seg0.ts"); n != 1 {
		t.Errorf("segment 0 was fetched %d times; it was already saved", n)
	}
	if got, _ := os.ReadFile(before.Path); sum(got) != sum(joined(segs)) {
		t.Error("the file put together from two addresses does not match the stream")
	}
}

// Waiting in the queue must not cost a stream its segments either.
func TestANewAddressContinuesAStreamAfterWaitingInTheQueue(t *testing.T) {
	segs := makeSegments(12)
	s := newHLSServer(t, segs)
	dir := t.TempDir()
	m := NewManager(dir, 1)
	m.announce = nil
	id, before := pausedStream(t, m, s)
	blocker := holdTheQueue(t, m)

	if err := m.ChangeAddress(id, s.URL+"/show.m3u8?sig=new"); err != nil {
		t.Fatal(err)
	}
	m.Pause(id)
	m.Pause(blocker)
	waitFor(t, 10*time.Second, "the other download to stop", stateIs(m, blocker, StatePaused))
	if !m.Resume(id) {
		t.Fatal("resume refused")
	}
	waitFor(t, 30*time.Second, "the stream to finish", stateIs(m, id, StateDone))
	if v, _ := findTask(m, id); v.Path != before.Path {
		t.Errorf("finished in %s, want %s continued", filepath.Base(v.Path), filepath.Base(before.Path))
	}
	if n := s.hitCount("/seg0.ts"); n != 1 {
		t.Errorf("segment 0 was fetched %d times; it was already saved", n)
	}
	if got, _ := os.ReadFile(before.Path); sum(got) != sum(joined(segs)) {
		t.Error("the file does not match the stream")
	}
}

// An address that is some other stream is refused, as another file is for a
// plain download: the segments saved are not thrown away for it, and it is not
// fetched into a second file beside them.
func TestANewAddressThatIsAnotherStreamIsRefused(t *testing.T) {
	s := newHLSServer(t, makeSegments(12))
	other := newHLSServer(t, makeSegments(7))
	dir := t.TempDir()
	m := NewManager(dir, 1)
	m.announce = nil
	id, before := pausedStream(t, m, s)
	partial, err := os.ReadFile(before.Path)
	if err != nil {
		t.Fatal(err)
	}

	if err := m.ChangeAddress(id, other.URL+"/show.m3u8"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "the download to stop one way or another", func() bool {
		v, _ := findTask(m, id)
		return v.State == StateNeedsRefresh || v.State == StateDone || v.State == StateError
	})
	v, _ := findTask(m, id)
	if v.State != StateNeedsRefresh || !strings.Contains(v.Error, "different stream") {
		t.Errorf("ended %s (%q) in %s, want the address refused as another stream",
			v.State, v.Error, filepath.Base(v.Path))
	}
	if got := namesIn(t, dir); len(got) != 2 || got[0] != "show.ts" || got[1] != "show.ts.godm" {
		t.Errorf("the folder holds %v, want the partial file and its sidecar and nothing else", got)
	}
	if after, _ := os.ReadFile(before.Path); sum(after) != sum(partial) {
		t.Error("the partial file was written to")
	}
	if st, ok := loadHLSState(before.Path); !ok || !strings.Contains(st.Playlist, "sig=old") {
		t.Error("the sidecar no longer describes the stream the segments came from")
	}
}
