package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shortTries makes a try stop after want bytes or after took, and looks at it
// often, for the length of the test.
func shortTries(t *testing.T, want int64, took time.Duration) {
	t.Helper()
	b, f, k := tryBytes, tryFor, tryTick
	tryBytes, tryFor, tryTick = want, took, 10*time.Millisecond
	t.Cleanup(func() { tryBytes, tryFor, tryTick = b, f, k })
}

// listFile serves payload slowly enough that a try can be stopped part of the
// way through it.
func listFile(t *testing.T, payload []byte) (*slowServer, string) {
	t.Helper()
	h := &slowServer{payload: payload, chunk: 8 << 10, delay: 10 * time.Millisecond}
	srv := newTestServer(t, h)
	return h, srv.URL + "/film.bin"
}

func listManager(t *testing.T) *Manager {
	t.Helper()
	m := NewManager(t.TempDir(), 2)
	m.announce = nil
	return m
}

func TestADownloadOnTheListIsTriedThenWaits(t *testing.T) {
	shortTries(t, 64<<10, time.Minute)
	m := listManager(t)
	payload := makePayload(1 << 20)
	_, link := listFile(t, payload)

	id, err := m.Add(jobRequest{URL: link, Connections: 1, Later: true})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the try to end on the list", stateIs(m, id, StateListed))
	v, _ := findTask(m, id)
	if !v.Later {
		t.Error("a task waiting on the list does not say it is on the list")
	}
	if v.Received < 64<<10 || v.Received >= int64(len(payload)) {
		t.Errorf("the try fetched %d bytes of %d, want at least %d and not all of it", v.Received, len(payload), 64<<10)
	}
	if v.Size != int64(len(payload)) || v.Path == "" {
		t.Errorf("the list does not know the file: size %d, path %q", v.Size, v.Path)
	}
	if v.TrySpeed <= 0 {
		t.Errorf("the try's speed is %d", v.TrySpeed)
	}
	// It stays there: nothing more is fetched until it is started.
	time.Sleep(300 * time.Millisecond)
	if again, _ := findTask(m, id); again.State != StateListed || again.Received != v.Received {
		t.Fatalf("a task on the list went on to %s with %d bytes", again.State, again.Received)
	}

	if n := m.StartList(); n != 1 {
		t.Fatalf("StartList started %d, want 1", n)
	}
	waitFor(t, 30*time.Second, "the download", stateIs(m, id, StateDone))
	v, _ = findTask(m, id)
	if v.Later {
		t.Error("a finished download is still on the list")
	}
	got, err := os.ReadFile(v.Path)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("the file after the try and the rest is wrong (%d bytes, err %v)", len(got), err)
	}
}

func TestATryOnASlowLinkStopsAtItsTime(t *testing.T) {
	shortTries(t, 1<<30, 400*time.Millisecond)
	m := listManager(t)
	payload := makePayload(4 << 20)
	_, link := listFile(t, payload)
	id, err := m.Add(jobRequest{URL: link, Connections: 1, Later: true})
	if err != nil {
		t.Fatal(err)
	}
	begun := time.Now()
	waitFor(t, 20*time.Second, "the try to end on the list", stateIs(m, id, StateListed))
	if took := time.Since(begun); took > 10*time.Second {
		t.Errorf("a try of 400 ms took %v", took)
	}
	if v, _ := findTask(m, id); v.Received <= 0 || v.Received >= int64(len(payload)) {
		t.Errorf("the try fetched %d of %d bytes", v.Received, len(payload))
	}
}

func TestAFileSmallEnoughToFinishInItsTryIsDone(t *testing.T) {
	shortTries(t, 1<<20, time.Minute)
	m := listManager(t)
	_, link := listFile(t, makePayload(32<<10))
	id, err := m.Add(jobRequest{URL: link, Connections: 1, Later: true})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the download", stateIs(m, id, StateDone))
	if v, _ := findTask(m, id); v.Later {
		t.Error("a finished download is still on the list")
	}
}

// A list built while downloads fill every slot is tried at once, and what is
// started from it then waits its turn in the queue like anything else.
func TestTriesDoNotWaitForTheQueue(t *testing.T) {
	shortTries(t, 64<<10, time.Minute)
	m := NewManager(t.TempDir(), 1)
	m.announce = nil
	m.mu.Lock()
	m.running = m.limit // every slot taken
	m.mu.Unlock()
	_, link := listFile(t, makePayload(2<<20))
	id, err := m.Add(jobRequest{URL: link, Connections: 1, Later: true})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the try, with the queue full", stateIs(m, id, StateListed))

	m.StartList()
	waitFor(t, 5*time.Second, "the started download to queue", stateIs(m, id, StateQueued))
	time.Sleep(200 * time.Millisecond)
	if v, _ := findTask(m, id); v.State != StateQueued {
		t.Fatalf("a download started from the list went to %s past a full queue", v.State)
	}
	m.mu.Lock()
	m.running = 0
	m.broadcastLocked()
	m.mu.Unlock()
	waitFor(t, 30*time.Second, "the download once a slot is free", stateIs(m, id, StateDone))
}

func TestResumeAllLeavesTheListAlone(t *testing.T) {
	shortTries(t, 64<<10, time.Minute)
	m := listManager(t)
	_, link := listFile(t, makePayload(2<<20))
	id, err := m.Add(jobRequest{URL: link, Connections: 1, Later: true})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the try", stateIs(m, id, StateListed))
	m.ResumeAll()
	time.Sleep(200 * time.Millisecond)
	if v, _ := findTask(m, id); v.State != StateListed {
		t.Fatalf("Resume all took a download off the list: %s", v.State)
	}
	// Starting it on its own takes it off the list.
	if !m.Resume(id) {
		t.Fatal("a download on the list could not be started")
	}
	waitFor(t, 30*time.Second, "the download", stateIs(m, id, StateDone))
}

func TestAPauseDuringATryIsAPause(t *testing.T) {
	shortTries(t, 1<<30, time.Minute)
	m := listManager(t)
	_, link := listFile(t, makePayload(4<<20))
	id, err := m.Add(jobRequest{URL: link, Connections: 1, Later: true})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the try to run", func() bool {
		v, _ := findTask(m, id)
		return v.State == StateRunning && v.Received > 0
	})
	m.Pause(id)
	waitFor(t, 10*time.Second, "the pause", stateIs(m, id, StatePaused))
	if v, _ := findTask(m, id); !v.Later {
		t.Error("a paused try left the list")
	}
}

func TestAskingForADownloadOnTheListStartsIt(t *testing.T) {
	shortTries(t, 64<<10, time.Minute)
	m := listManager(t)
	_, link := listFile(t, makePayload(2<<20))
	id, err := m.Add(jobRequest{URL: link, Connections: 1, Later: true})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the try", stateIs(m, id, StateListed))
	if again, _ := m.Add(jobRequest{URL: link, Connections: 1, Later: true}); again != id {
		t.Errorf("putting it on the list twice made a second task %s", again)
	}
	if v, _ := findTask(m, id); v.State != StateListed {
		t.Errorf("putting it on the list again started it: %s", v.State)
	}
	now, err := m.Add(jobRequest{URL: link, Connections: 1})
	if err != nil || now != id {
		t.Fatalf("asking for it now gave %s (%v), want the one on the list", now, err)
	}
	waitFor(t, 30*time.Second, "the download", stateIs(m, id, StateDone))
	if n := len(m.List()); n != 1 {
		t.Errorf("there are %d tasks, want 1", n)
	}
}

func TestTheListIsKeptAcrossARestart(t *testing.T) {
	shortTries(t, 64<<10, time.Minute)
	dir := t.TempDir()
	store := filepath.Join(dir, "tasks.json")
	m := NewManager(dir, 2)
	m.announce, m.store = nil, store
	_, link := listFile(t, makePayload(2<<20))
	waiting, err := m.Add(jobRequest{URL: link, Connections: 1, Later: true})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the try", stateIs(m, waiting, StateListed))
	// And one whose try was under way when godm stopped, and one that had not
	// been tried yet.
	cut := m.get(waiting).snapshot()
	cut.ID, cut.State = "t-cut", StateRunning
	untried := cut
	untried.ID, untried.State, untried.Received = "t-untried", StateQueued, 0
	m.mu.Lock()
	m.tasks[cut.ID] = &managedTask{view: cut, req: jobRequest{URL: link + "?2", Later: true}}
	m.tasks[untried.ID] = &managedTask{view: untried, req: jobRequest{URL: link + "?3", Later: true}}
	m.order = append(m.order, cut.ID, untried.ID)
	m.mu.Unlock()
	if err := m.save(); err != nil {
		t.Fatal(err)
	}

	m2 := NewManager(dir, 2)
	m2.announce, m2.store = nil, store
	if err := m2.load(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{waiting, "t-cut"} {
		v, ok := findTask(m2, id)
		if !ok || v.State != StateListed || !v.Later {
			t.Errorf("after a restart %s is %s, on the list %v; want it waiting on the list", id, v.State, v.Later)
		}
	}
	if v, ok := findTask(m2, "t-untried"); !ok || v.State != StatePaused || !v.Later {
		t.Errorf("after a restart one never tried is %s, on the list %v; want it paused on the list", v.State, v.Later)
	}
}

func TestTheListStartsAtTheTimeSet(t *testing.T) {
	shortTries(t, 64<<10, time.Minute)
	m := listManager(t)
	_, link := listFile(t, makePayload(2<<20))
	id, err := m.Add(jobRequest{URL: link, Connections: 1, Later: true})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the try", stateIs(m, id, StateListed))

	at, daily := "02:30", true
	if err := m.UpdateSettings(settingsUpdate{ListAt: &at, ListDaily: &daily}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 25, 1, 0, 0, 0, time.Local)
	m.mu.Lock()
	m.planListLocked(base)
	due := m.listNext
	m.mu.Unlock()
	if want := time.Date(2026, 10, 25, 2, 30, 0, 0, time.Local); !due.Equal(want) {
		t.Fatalf("the list is due at %v, want %v", due, want)
	}
	m.listTick(due.Add(-time.Second))
	if v, _ := findTask(m, id); v.State != StateListed {
		t.Fatalf("the list started before its time: %s", v.State)
	}
	m.listTick(due.Add(20 * time.Second))
	waitFor(t, 30*time.Second, "the list to start and finish", stateIs(m, id, StateDone))
	if next := m.ListNext(); !next.Equal(due.AddDate(0, 0, 1)) {
		t.Errorf("a daily start is next due at %v, want %v", next, due.AddDate(0, 0, 1))
	}

	// Once: it starts, then the time is cleared.
	once := false
	if err := m.UpdateSettings(settingsUpdate{ListDaily: &once}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.planListLocked(base)
	m.mu.Unlock()
	m.listTick(due.Add(time.Minute))
	if got, next := m.listAt(), m.ListNext(); got != "" || !next.IsZero() {
		t.Errorf("after a start made once the time is %q, next %v; want none", got, next)
	}
}

func TestAStartMissedByMuchIsNotMadeLate(t *testing.T) {
	shortTries(t, 64<<10, time.Minute)
	m := listManager(t)
	_, link := listFile(t, makePayload(2<<20))
	id, err := m.Add(jobRequest{URL: link, Connections: 1, Later: true})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the try", stateIs(m, id, StateListed))
	at, daily := "02:30", true
	if err := m.UpdateSettings(settingsUpdate{ListAt: &at, ListDaily: &daily}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 25, 1, 0, 0, 0, time.Local)
	m.mu.Lock()
	m.planListLocked(base)
	due := m.listNext
	m.mu.Unlock()
	// The computer slept through it and woke at nine.
	m.listTick(due.Add(6*time.Hour + 30*time.Minute))
	time.Sleep(200 * time.Millisecond)
	if v, _ := findTask(m, id); v.State != StateListed {
		t.Fatalf("a start missed by hours was made late: %s", v.State)
	}
	if next := m.ListNext(); !next.After(due.Add(6 * time.Hour)) {
		t.Errorf("the next start is %v, want tomorrow's", next)
	}
}

func TestTheListAPI(t *testing.T) {
	shortTries(t, 64<<10, time.Minute)
	m := listManager(t)
	call := settingsAPI(t, m)
	for _, bad := range []string{`{"list_at":"25:00"}`, `{"list_at":"2pm"}`} {
		if code, _ := call("POST", bad, "tok"); code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad, code)
		}
	}
	code, got := call("POST", `{"list_at":" 7:05 ","list_daily":true}`, "tok")
	if code != 200 || got["list_at"] != "07:05" || got["list_daily"] != true || got["list_next"] == "0001-01-01T00:00:00Z" {
		t.Fatalf("POST = %d %v, want 07:05 every day with a next start", code, got)
	}
	if _, got = call("POST", `{"list_at":""}`, "tok"); got["list_at"] != "" || got["list_next"] != "0001-01-01T00:00:00Z" {
		t.Errorf("clearing the time left %v", got)
	}

	s := &server{mgr: m, token: "tok"}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/list/start", s.guard(post(s.handleListStart)))
	mux.HandleFunc("/api/download", s.guard(post(s.handleDownload)))
	srv := newTestServer(t, mux)
	do := func(path, body string) map[string]any {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return out
	}
	_, link := listFile(t, makePayload(2<<20))
	added := do("/api/download", `{"url":"`+link+`","connections":1,"later":true}`)
	id, _ := added["id"].(string)
	waitFor(t, 20*time.Second, "the try", stateIs(m, id, StateListed))
	if got := do("/api/list/start", ""); got["started"] != float64(1) {
		t.Errorf("/api/list/start = %v, want one started", got)
	}
	waitFor(t, 30*time.Second, "the download", stateIs(m, id, StateDone))
}

// A run that was called off ends on the list only if its try stopped it, and a
// try taken off the list as it stopped runs again, in the queue. The decision
// is made under the lock that writes the state, so a start from the list in
// between cannot leave the task waiting on a list it is no longer on.
func TestARunStoppedForTheListEndsWhereItShould(t *testing.T) {
	m := listManager(t)
	end := func(later bool, held int) (TaskState, bool, bool) {
		mt := &managedTask{gen: 3, view: TaskView{ID: "x", State: StateRunning}}
		mt.req.Later, mt.view.Later, mt.tryHeld = later, later, held
		again := m.finishRun(mt, 3, StatePaused, "", "")
		return mt.view.State, mt.view.Later, again
	}
	if st, _, again := end(true, 0); st != StatePaused || again {
		t.Errorf("a try stopped by a pause = %s, again %v", st, again)
	}
	if st, on, again := end(true, 3); st != StateListed || !on || again {
		t.Errorf("a try that did its part = %s, on the list %v, again %v", st, on, again)
	}
	if st, on, again := end(false, 3); st != StatePaused || on || !again {
		t.Errorf("a try started as it stopped = %s, on the list %v, again %v; want it run again", st, on, again)
	}
}

// Starting a download while it is being tried moves it to the queue, which it
// waits in like anything else, and frees its lane of tries.
func TestStartingATryMovesItToTheQueue(t *testing.T) {
	shortTries(t, 1<<30, time.Minute)
	m := NewManager(t.TempDir(), 1)
	m.announce = nil
	m.mu.Lock()
	m.running = m.limit // every slot taken
	m.mu.Unlock()
	_, link := listFile(t, makePayload(4<<20))
	id, err := m.Add(jobRequest{URL: link, Connections: 1, Later: true})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the try to run", func() bool {
		v, _ := findTask(m, id)
		return v.State == StateRunning && v.Received > 0
	})
	if n := m.StartList(); n != 1 {
		t.Fatalf("StartList started %d, want 1", n)
	}
	waitFor(t, 10*time.Second, "the started download to join the queue", stateIs(m, id, StateQueued))
	time.Sleep(200 * time.Millisecond)
	v, _ := findTask(m, id)
	m.mu.Lock()
	trying := m.trying
	m.mu.Unlock()
	if v.State != StateQueued || v.Later || trying != 0 {
		t.Fatalf("after starting a try it is %s, on the list %v, with %d tries running; want it queued, off the list, none", v.State, v.Later, trying)
	}
	m.mu.Lock()
	m.running = 0
	m.broadcastLocked()
	m.mu.Unlock()
	waitFor(t, 30*time.Second, "the download once a slot is free", stateIs(m, id, StateDone))
}

// Pause all and then Resume all leaves the list as it was: a try that was
// paused is tried again, not downloaded in full.
func TestResumeAllTriesAgainWhatWasPausedOnTheList(t *testing.T) {
	shortTries(t, 1<<30, time.Second)
	m := listManager(t)
	_, link := listFile(t, makePayload(4<<20))
	id, err := m.Add(jobRequest{URL: link, Connections: 1, Later: true})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the try to run", stateIs(m, id, StateRunning))
	m.PauseAll()
	waitFor(t, 10*time.Second, "the pause", stateIs(m, id, StatePaused))
	m.ResumeAll()
	waitFor(t, 20*time.Second, "the try again, ending on the list", stateIs(m, id, StateListed))
	if v, _ := findTask(m, id); !v.Later {
		t.Error("Resume all took the download off the list")
	}
}

// A torrent whose peers never answer gives up its try once it has waited a
// while for the list of its files, and waits on the list for the rest.
func TestATorrentThatFindsNoPeersEndsItsTry(t *testing.T) {
	saved := tryMetaFor
	tryMetaFor = 300 * time.Millisecond
	t.Cleanup(func() { tryMetaFor = saved })
	shortTries(t, 64<<10, time.Minute)
	m := newBTManager(t)
	id, err := m.Add(jobRequest{URL: "magnet:?xt=urn:btih:" + strings.Repeat("ab", 20) + "&dn=nobody", Later: true})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the try to give up on the metadata", stateIs(m, id, StateListed))
	if v, _ := findTask(m, id); !v.Later {
		t.Error("a torrent that found no peers left the list")
	}
}
