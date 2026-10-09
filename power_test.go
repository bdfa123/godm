package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

// fakeAwake stands in for the operating system's keep-awake request.
type fakeAwake struct {
	mu    sync.Mutex
	calls []bool
}

func (f *fakeAwake) set(on bool) error {
	f.mu.Lock()
	f.calls = append(f.calls, on)
	f.mu.Unlock()
	return nil
}

func (f *fakeAwake) history() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.calls...)
}

// holding is true while the last thing asked for was to stay awake.
func (f *fakeAwake) holding() bool {
	h := f.history()
	return len(h) > 0 && h[len(h)-1]
}

// managerWithFakePower connects a manager to a fake, and withdraws the fake
// request when the test ends so nothing outlives it.
func managerWithFakePower(t *testing.T, parallel int) (*Manager, *fakeAwake) {
	t.Helper()
	m := NewManager(t.TempDir(), parallel)
	f := &fakeAwake{}
	m.SetPower(powerOps{keepAwake: f.set})
	t.Cleanup(func() { m.SetPower(powerOps{}) })
	return m, f
}

func newTestServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// slowFile serves a file that takes about a second, long enough to look at the
// machine while the download runs.
func slowFile(t *testing.T) string {
	t.Helper()
	srv := newTestServer(t, &slowServer{payload: makePayload(256 << 10), chunk: 16 << 10, delay: 60 * time.Millisecond})
	return srv.URL + "/slow.bin"
}

func TestKeepAwakeHoldsOnlyWhileADownloadRuns(t *testing.T) {
	m, f := managerWithFakePower(t, 2)
	time.Sleep(100 * time.Millisecond)
	if h := f.history(); len(h) != 0 {
		t.Fatalf("idle manager touched the power state: %v", h)
	}

	id, err := m.Add(jobRequest{URL: slowFile(t), Connections: 1})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the request to stay awake", f.holding)
	if v, _ := findTask(m, id); v.State == StateDone {
		t.Fatal("the download finished before the request could be observed; the test proves nothing")
	}

	waitFor(t, 20*time.Second, "the download", stateIs(m, id, StateDone))
	waitFor(t, 5*time.Second, "the request to be withdrawn", func() bool { return !f.holding() })
	if got, want := f.history(), []bool{true, false}; !reflect.DeepEqual(got, want) {
		t.Errorf("power requests = %v, want %v", got, want)
	}
}

func TestKeepAwakeEndsWhenTheDownloadIsPaused(t *testing.T) {
	m, f := managerWithFakePower(t, 2)
	id, _ := m.Add(jobRequest{URL: slowFile(t), Connections: 1})
	waitFor(t, 10*time.Second, "the request to stay awake", f.holding)

	m.Pause(id)
	waitFor(t, 10*time.Second, "the request to be withdrawn", func() bool { return !f.holding() })
}

func TestKeepAwakeCanBeSwitchedOff(t *testing.T) {
	m, f := managerWithFakePower(t, 2)
	off, on := false, true

	// Off before anything starts: a whole download goes by without a request.
	if err := m.UpdateSettings(settingsUpdate{KeepAwake: &off}); err != nil {
		t.Fatal(err)
	}
	id, _ := m.Add(jobRequest{URL: slowFile(t), Connections: 1})
	waitFor(t, 20*time.Second, "the download", stateIs(m, id, StateDone))
	time.Sleep(100 * time.Millisecond)
	if h := f.history(); len(h) != 0 {
		t.Fatalf("asked the system to stay awake with the setting off: %v", h)
	}

	// Switched on and off again while a download is running.
	id, _ = m.Add(jobRequest{URL: slowFile(t) + "?second", Filename: "second.bin", Connections: 1})
	if err := m.UpdateSettings(settingsUpdate{KeepAwake: &on}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the request once switched on", f.holding)
	if err := m.UpdateSettings(settingsUpdate{KeepAwake: &off}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the request to be withdrawn once switched off", func() bool { return !f.holding() })
	m.Pause(id)
	waitFor(t, 10*time.Second, "the pause", stateIs(m, id, StatePaused)) // the file is closed by then
}

func TestAwakeKeeperWithdrawsItsRequestWhenStopped(t *testing.T) {
	f := &fakeAwake{}
	k := newAwakeKeeper(f.set)
	k.Want(true)
	waitFor(t, 5*time.Second, "the request", f.holding)
	k.Stop()
	if f.holding() {
		t.Fatalf("Stop returned with the request still standing: %v", f.history())
	}
}

func TestAwakeKeeperNeverRepeatsItself(t *testing.T) {
	f := &fakeAwake{}
	k := newAwakeKeeper(f.set)
	for i := 0; i < 5; i++ {
		k.Want(true)
	}
	waitFor(t, 5*time.Second, "the request", f.holding)
	k.Want(true)
	k.Want(false)
	k.Want(false)
	k.Stop()
	if got, want := f.history(), []bool{true, false}; !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

// ---------- persistence ----------

func TestKeepAwakeDefaultsToOnAndTheChoiceSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "tasks.json")

	// A list written before settings existed must not switch the feature off.
	if err := os.WriteFile(store, []byte(`{"version":1,"limit":3,"tasks":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(dir, 2)
	m.store = store
	if err := m.load(); err != nil {
		t.Fatal(err)
	}
	if !m.KeepAwake() {
		t.Fatal("an older task list turned keep-awake off")
	}

	off := false
	if err := m.UpdateSettings(settingsUpdate{KeepAwake: &off}); err != nil {
		t.Fatal(err)
	}
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	m2 := NewManager(dir, 2)
	m2.store = store
	if err := m2.load(); err != nil {
		t.Fatal(err)
	}
	if m2.KeepAwake() {
		t.Error("the choice to turn keep-awake off was not restored")
	}
}
