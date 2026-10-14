package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// longerSegments serves s with every segment declared six seconds long instead
// of four: a stream with as many segments as s and a different running time.
func longerSegments(t *testing.T, s *hlsServer) *httptest.Server {
	t.Helper()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		s.serve(rec, r)
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		w.Write([]byte(strings.ReplaceAll(rec.Body.String(), "#EXTINF:4.0,", "#EXTINF:6.0,")))
	}))
	t.Cleanup(proxy.Close)
	return proxy
}

// Two streams can have as many segments as each other, and a quality to match,
// without being the same video. The address of one is refused for a download
// of the other when they are not as long.
func TestANewAddressForAStreamOfAnotherLengthIsRefused(t *testing.T) {
	s := newHLSServer(t, makeSegments(12))
	dir := t.TempDir()
	m := NewManager(dir, 1)
	m.announce = nil
	id, before := pausedStream(t, m, s)
	partial, err := os.ReadFile(before.Path)
	if err != nil {
		t.Fatal(err)
	}

	if err := m.ChangeAddress(id, longerSegments(t, s).URL+"/show.m3u8"); err != nil {
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
}

// The run that takes a stream's new address up can fail before it has written
// a segment, when the file will not open. It has made its plan by then, and the
// sidecar has to name the new address all the same for the run after it.
func TestAStreamsNewAddressSurvivesAFileThatWillNotOpen(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens read-only files all the same")
	}
	segs := makeSegments(12)
	s := newHLSServer(t, segs)
	dir := t.TempDir()
	m := NewManager(dir, 1)
	m.announce = nil
	id, before := pausedStream(t, m, s)
	if err := os.Chmod(before.Path, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(before.Path, 0o644) })

	if err := m.ChangeAddress(id, s.URL+"/show.m3u8?sig=new"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the first try to fail", stateIs(m, id, StateError))
	if err := os.Chmod(before.Path, 0o644); err != nil {
		t.Fatal(err)
	}
	if !m.Resume(id) {
		t.Fatal("retry refused")
	}
	waitFor(t, 30*time.Second, "the stream to finish", stateIs(m, id, StateDone))
	if v, _ := findTask(m, id); v.Path != before.Path {
		t.Errorf("finished in %s, want %s continued", filepath.Base(v.Path), filepath.Base(before.Path))
	}
	if got := namesIn(t, dir); len(got) != 1 || got[0] != "show.ts" {
		t.Errorf("the folder holds %v, want show.ts alone", got)
	}
}

// A sidecar written by a version that did not note how long the stream plays
// is not held to it.
func TestANewAddressStillContinuesAStreamSavedWithoutItsLength(t *testing.T) {
	segs := makeSegments(12)
	s := newHLSServer(t, segs)
	dir := t.TempDir()
	m := NewManager(dir, 1)
	m.announce = nil
	id, before := pausedStream(t, m, s)
	st, ok := loadHLSState(before.Path)
	if !ok || st.Duration <= 0 {
		t.Fatalf("the sidecar does not say how long the stream is: %+v", st)
	}
	st.Duration = 0
	if err := saveHLSState(before.Path, st); err != nil {
		t.Fatal(err)
	}

	if err := m.ChangeAddress(id, longerSegments(t, s).URL+"/show.m3u8"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "the stream to finish", stateIs(m, id, StateDone))
	if v, _ := findTask(m, id); v.Path != before.Path {
		t.Errorf("finished in %s, want %s continued", filepath.Base(v.Path), filepath.Base(before.Path))
	}
	if got, _ := os.ReadFile(before.Path); sum(got) != sum(joined(segs)) {
		t.Error("the file does not match the stream")
	}
}

// A new address is taken up by the first run that gets as far as the file,
// and that run can be cut short at any point after. Whatever it managed, the
// run after it has to find the address the sidecar was last written with.

func TestANewAddressPausedTheMomentItRunsStillContinues(t *testing.T) {
	h, src := newTwoLinks(t)
	dir := t.TempDir()
	m := NewManager(dir, 1)
	m.announce = nil
	id, before := pausedPartway(t, m, src.URL)

	if err := m.ChangeAddress(id, src.URL+"/b/film.bin"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the run to begin", stateIs(m, id, StateRunning))
	m.Pause(id)
	waitFor(t, 10*time.Second, "the pause", stateIs(m, id, StatePaused))
	if !m.Resume(id) {
		t.Fatal("resume refused")
	}
	continued(t, m, id, before, h, dir)
}

func TestAStreamsNewAddressPausedTheMomentItRunsStillContinues(t *testing.T) {
	segs := makeSegments(12)
	s := newHLSServer(t, segs)
	dir := t.TempDir()
	m := NewManager(dir, 1)
	m.announce = nil
	id, before := pausedStream(t, m, s)
	s.mu.Lock()
	for i := 3; i < len(s.segs); i++ {
		s.delay[i] = 300 * time.Millisecond
	}
	s.mu.Unlock()

	if err := m.ChangeAddress(id, s.URL+"/show.m3u8?sig=new"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the run to begin", stateIs(m, id, StateRunning))
	m.Pause(id)
	waitFor(t, 10*time.Second, "the pause", stateIs(m, id, StatePaused))
	s.mu.Lock()
	s.delay = map[int]time.Duration{}
	s.mu.Unlock()
	if !m.Resume(id) {
		t.Fatal("resume refused")
	}
	waitFor(t, 30*time.Second, "the stream to finish", stateIs(m, id, StateDone))
	if v, _ := findTask(m, id); v.Path != before.Path {
		t.Errorf("finished in %s, want %s continued", filepath.Base(v.Path), filepath.Base(before.Path))
	}
	if got := namesIn(t, dir); len(got) != 1 || got[0] != "show.ts" {
		t.Errorf("the folder holds %v, want show.ts alone", got)
	}
}
