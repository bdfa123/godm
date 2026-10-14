package main

import (
	"bytes"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// "Also delete the file" when something other than godm has the file open: the
// player that Play started on a finished film, a virus scan, an archive tool.
// Windows will not delete such a file, and what these tests hold open is held
// the way most programs hold a file, which is the way Go opens one.

// heldOpen opens path as another program would. Where that does not keep a
// file from being deleted there is nothing to test.
func heldOpen(t *testing.T, path string) *os.File {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("only Windows refuses to delete a file that is open")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// logged collects what is logged during a test.
type logged struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logged) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logged) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func captureLog(t *testing.T) *logged {
	t.Helper()
	l := &logged{}
	log.SetOutput(l)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return l
}

func finishedDownload(t *testing.T, m *Manager) TaskView {
	t.Helper()
	src := httptest.NewServer(&rangedHandler{payload: makePayload(256 << 10)})
	t.Cleanup(src.Close)
	id, err := m.Add(jobRequest{URL: src.URL + "/film.mkv", Connections: 2})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the download to finish", stateIs(m, id, StateDone))
	v, _ := findTask(m, id)
	return v
}

func TestDeletingAFileWaitsForTheProgramThatHasItOpen(t *testing.T) {
	m := NewManager(t.TempDir(), 2)
	m.announce = nil
	v := finishedDownload(t, m)
	player := heldOpen(t, v.Path)

	if !m.Remove(v.ID, true) {
		t.Fatal("the download could not be removed")
	}
	if !exists(v.Path) {
		t.Skip("this system deleted the file while it was open")
	}
	// The player moves on to something else.
	time.Sleep(300 * time.Millisecond)
	player.Close()
	waitFor(t, 10*time.Second, "the file to be deleted once nothing has it open", func() bool {
		return !exists(v.Path)
	})
}

// A file that never comes free in the time allowed stays, and then so does its
// sidecar: a partial file without one cannot be continued by anything. The log
// says which file it was.
func TestAFileThatCannotBeDeletedKeepsItsSidecar(t *testing.T) {
	restore := deleteFor
	deleteFor = 400 * time.Millisecond
	t.Cleanup(func() { deleteFor = restore })
	said := captureLog(t)

	_, src := newTwoLinks(t)
	m := NewManager(t.TempDir(), 2)
	m.announce = nil
	id, v := pausedPartway(t, m, src.URL)
	heldOpen(t, v.Path)

	if !m.Remove(id, true) {
		t.Fatal("the download could not be removed")
	}
	if !exists(v.Path) {
		t.Skip("this system deleted the file while it was open")
	}
	const orphaned = "the file is still there and its sidecar was deleted: what is left cannot be continued"
	if !exists(statePath(v.Path)) {
		t.Fatal(orphaned)
	}
	waitFor(t, 10*time.Second, "godm to say that it could not delete the file", func() bool {
		return strings.Contains(said.String(), filepath.Base(v.Path))
	})
	if !exists(v.Path) {
		t.Fatal("the file went after all")
	}
	if !exists(statePath(v.Path)) {
		t.Error(orphaned)
	}
}

// While a file waits to be deleted it is nobody's to continue. A download of
// the same link added in that time would otherwise find the partial file and
// its sidecar, take them for its own, and write into a file that is deleted
// under it the moment it comes free.
func TestAFileOnItsWayOutIsNotPickedUpByANewDownload(t *testing.T) {
	h, src := newTwoLinks(t)
	dir := t.TempDir()
	m := NewManager(dir, 2)
	m.announce = nil
	id, v := pausedPartway(t, m, src.URL)
	scan := heldOpen(t, v.Path)

	if !m.Remove(id, true) {
		t.Fatal("the download could not be removed")
	}
	if !exists(v.Path) {
		t.Skip("this system deleted the file while it was open")
	}
	again, err := m.Add(jobRequest{URL: v.URL, Connections: 2})
	if err != nil {
		t.Fatal(err)
	}
	var path string
	waitFor(t, 20*time.Second, "the new download to choose its file", func() bool {
		nv, _ := findTask(m, again)
		path = nv.Path
		return path != ""
	})
	if path == v.Path {
		t.Errorf("the new download continued %s, which is being deleted", filepath.Base(v.Path))
	}
	scan.Close()
	waitFor(t, 10*time.Second, "the removed download's file and sidecar to go", func() bool {
		return !exists(v.Path) && !exists(statePath(v.Path))
	})
	waitFor(t, 30*time.Second, "the new download to finish", stateIs(m, again, StateDone))
	nv, _ := findTask(m, again)
	got, err := os.ReadFile(nv.Path)
	if err != nil {
		t.Fatalf("the new download's file is gone: %v", err)
	}
	if sum(got) != sum(h.file.payload) {
		t.Error("the new download does not match the source")
	}
}
