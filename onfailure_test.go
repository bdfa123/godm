package main

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// refuser serves a file, or turns every request away with 400 while refuse is
// set, which the engine takes as a failure at once.
type refuser struct {
	refuse atomic.Bool
	asked  atomic.Int32
	body   []byte
}

func (h *refuser) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.asked.Add(1)
	if h.refuse.Load() {
		http.Error(w, "not now", http.StatusBadRequest)
		return
	}
	http.ServeContent(w, r, "file.bin", time.Time{}, bytes.NewReader(h.body))
}

func refusing(t *testing.T) (*refuser, string) {
	h := &refuser{body: makePayload(64 << 10)}
	h.refuse.Store(true)
	return h, newTestServer(t, h).URL + "/file.bin"
}

// failRun ends the run under way of a task put in with taskIn, failed, with got
// bytes of it on disk.
func failRun(m *Manager, mt *managedTask, st TaskState, got int64) TaskView {
	mt.mu.Lock()
	mt.view.Received = got
	gen := mt.gen
	mt.mu.Unlock()
	m.finishRun(mt, gen, st, "broken", "")
	v, _ := findTask(m, mt.view.ID)
	return v
}

// tryAgain stands in for the try again of a task put in with taskIn: what rerun
// does to it, with no download behind it.
func tryAgain(t *testing.T, mt *managedTask) {
	t.Helper()
	mt.mu.Lock()
	defer mt.mu.Unlock()
	if mt.view.RetryAt.IsZero() {
		t.Fatalf("%s is not to be tried again", mt.view.ID)
	}
	mt.gen++
	mt.autoGen = mt.gen
	mt.view.Retries++
	mt.view.RetryAt = time.Time{}
	mt.view.State = StateRunning
}

func settingsInts(every, times int) settingsUpdate {
	return settingsUpdate{RetryEvery: &every, RetryTimes: &times}
}

// ---------- with real downloads ----------

func TestAFailedDownloadIsTriedAgainWhenItsTimeComes(t *testing.T) {
	m, _, n := managerWithFakePC(t, 2)
	h, link := refusing(t)
	before := time.Now()
	id, err := m.Add(jobRequest{URL: link, Connections: 1})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the download to fail", stateIs(m, id, StateError))
	v, _ := findTask(m, id)
	if v.RetryAt.Before(before.Add(5*time.Minute)) || v.RetryAt.After(time.Now().Add(5*time.Minute)) {
		t.Fatalf("to be tried again at %v, want five minutes after it failed (%v)", v.RetryAt, before)
	}
	waitFor(t, 2*time.Second, "the notice", func() bool { return len(n.withTitle("Download failed")) == 1 })
	if got := n.withTitle("Download failed")[0]; !strings.HasSuffix(got, " godm tries it again in 5 min.") {
		t.Errorf("notice = %q, want it to say when the download is tried again", got)
	}

	// Before its time nothing is asked of the server.
	asked := h.asked.Load()
	m.retryTick(v.RetryAt.Add(-time.Second))
	time.Sleep(200 * time.Millisecond)
	if now, _ := findTask(m, id); now.State != StateError || h.asked.Load() != asked {
		t.Fatalf("before its time the download is %s and the server was asked %d more times", now.State, h.asked.Load()-asked)
	}

	h.refuse.Store(false)
	m.retryTick(v.RetryAt)
	waitFor(t, 20*time.Second, "the try again to finish it", stateIs(m, id, StateDone))
	if v, _ := findTask(m, id); !v.RetryAt.IsZero() {
		t.Errorf("finished, and still to be tried again at %v", v.RetryAt)
	}
}

// A try again that fails as the run before it did is counted; when the tries
// are used up the download is given up on, which is what an action armed
// waits for. The user hears of the failure and of giving up, not of each try.
func TestTriesAgainThatComeToNothingAreCountedThenGivenUp(t *testing.T) {
	m, pc, n := managerWithFakePC(t, 2)
	if err := m.UpdateSettings(settingsInts(5, 2)); err != nil {
		t.Fatal(err)
	}
	_, link := refusing(t)
	arm(t, m, "sleep")
	id, err := m.Add(jobRequest{URL: link, Connections: 1})
	if err != nil {
		t.Fatal(err)
	}
	failedAfter := func(tries int, more bool) func() bool {
		return func() bool {
			v, _ := findTask(m, id)
			return v.State == StateError && v.Retries == tries && v.RetryAt.IsZero() != more
		}
	}
	waitFor(t, 20*time.Second, "the first failure", failedAfter(0, true))
	nothingHappens(t, pc, "the download is to be tried again")
	for try := 1; try <= 2; try++ {
		v, _ := findTask(m, id)
		m.retryTick(v.RetryAt)
		waitFor(t, 20*time.Second, fmt.Sprintf("try again %d to fail", try), failedAfter(try, try < 2))
		if try < 2 {
			nothingHappens(t, pc, "a try again is still to come")
		}
	}
	happensOnce(t, pc, "sleep")
	waitFor(t, 2*time.Second, "the notice of giving up", func() bool { return len(n.withTitle("Download failed")) == 2 })
	got := n.withTitle("Download failed")
	if !strings.HasSuffix(got[0], "again in 5 min.") || !strings.HasSuffix(got[1], "Tried again 2 times without getting further: godm has given up on it.") {
		t.Errorf("notices = %q, want the failure and then the giving up", got)
	}
}

func TestStartingADownloadByHandCallsOffItsTryAgain(t *testing.T) {
	m, _, _ := managerWithFakePC(t, 2)
	h, link := refusing(t)
	id, err := m.Add(jobRequest{URL: link, Connections: 1})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the download to fail", stateIs(m, id, StateError))
	planned, _ := findTask(m, id)
	m.mu.Lock()
	mt := m.tasks[id]
	m.mu.Unlock()
	mt.mu.Lock()
	mt.view.Retries = 2 // as if tries again had come to nothing before
	mt.mu.Unlock()

	h.refuse.Store(false)
	if !m.Resume(id) {
		t.Fatal("resume refused")
	}
	if v, _ := findTask(m, id); !v.RetryAt.IsZero() || v.Retries != 0 {
		t.Errorf("started by hand, the download is still to be tried again at %v, %d counted", v.RetryAt, v.Retries)
	}
	waitFor(t, 20*time.Second, "the download", stateIs(m, id, StateDone))
	asked := h.asked.Load()
	m.retryTick(planned.RetryAt.Add(time.Hour))
	time.Sleep(200 * time.Millisecond)
	if v, _ := findTask(m, id); v.State != StateDone || h.asked.Load() != asked {
		t.Errorf("at the time of the try again it was called off for, the download is %s and the server was asked %d more times",
			v.State, h.asked.Load()-asked)
	}
}

// A new address given by hand is a start by hand too: what was planned and
// counted goes, and nothing shows the download as waiting for a try again
// while it runs.
func TestChangingTheAddressCallsOffTheTryAgain(t *testing.T) {
	m, _, _ := managerWithFakePC(t, 2)
	h, link := refusing(t)
	id, err := m.Add(jobRequest{URL: link, Connections: 1})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the download to fail", stateIs(m, id, StateError))
	m.mu.Lock()
	mt := m.tasks[id]
	m.mu.Unlock()
	mt.mu.Lock()
	mt.view.Retries = 2
	mt.mu.Unlock()

	h.refuse.Store(false)
	if err := m.ChangeAddress(id, link+"?again"); err != nil {
		t.Fatal(err)
	}
	if v, _ := findTask(m, id); !v.RetryAt.IsZero() || v.Retries != 0 {
		t.Errorf("started on a new address, the download is to be tried again at %v, %d counted", v.RetryAt, v.Retries)
	}
	waitFor(t, 20*time.Second, "the download", stateIs(m, id, StateDone))
}

// listRefuser serves a file slowly enough for a try to stop part of the way,
// or refuses it, as refuser does.
type listRefuser struct {
	refuse atomic.Bool
	file   *slowServer
}

func (h *listRefuser) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.refuse.Load() {
		http.Error(w, "not now", http.StatusBadRequest)
		return
	}
	h.file.ServeHTTP(w, r)
}

func TestADownloadOnTheListIsTriedAgainOnTheList(t *testing.T) {
	shortTries(t, 64<<10, time.Minute)
	m, _, _ := managerWithFakePC(t, 2)
	payload := makePayload(1 << 20)
	h := &listRefuser{file: &slowServer{payload: payload, chunk: 8 << 10, delay: 10 * time.Millisecond}}
	h.refuse.Store(true)
	link := newTestServer(t, h).URL + "/film.bin"
	id, err := m.Add(jobRequest{URL: link, Connections: 1, Later: true})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "its try to fail", stateIs(m, id, StateError))
	v, _ := findTask(m, id)
	h.refuse.Store(false)
	m.retryTick(v.RetryAt)
	waitFor(t, 20*time.Second, "the try again to end on the list", stateIs(m, id, StateListed))
	if v, _ := findTask(m, id); !v.Later || v.Received >= int64(len(payload)) {
		t.Errorf("tried again, the download is on the list: %v, with %d of %d bytes; want a try, on the list", v.Later, v.Received, len(payload))
	}
}

// A try that has done its part finishes nothing, but once it no longer holds
// the action back, the action goes ahead for what did finish.
func TestATryThatEndsOnTheListLetsTheActionGoAhead(t *testing.T) {
	m, pc, _ := managerWithFakePC(t, 2)
	done, tried := taskIn(m, "done", StateRunning), taskIn(m, "tried", StateRunning)
	tried.mu.Lock()
	tried.req.Later, tried.view.Later, tried.tryHeld = true, true, 1
	tried.mu.Unlock()
	arm(t, m, "sleep")
	end(m, done, StateDone)
	nothingHappens(t, pc, "a try is still running")
	end(m, tried, StatePaused) // as watchTry stops it
	if v, _ := findTask(m, "tried"); v.State != StateListed {
		t.Fatalf("the try ended %s, want it on the list", v.State)
	}
	happensOnce(t, pc, "sleep")
}

// ---------- counting ----------

func TestATryThatGetsFurtherStartsTheCountAgain(t *testing.T) {
	m, _, _ := managerWithFakePC(t, 2)
	if err := m.UpdateSettings(settingsInts(5, 1)); err != nil {
		t.Fatal(err)
	}
	mt := taskIn(m, "x", StateRunning)
	if v := failRun(m, mt, StateError, 100); v.RetryAt.IsZero() || v.Retries != 0 {
		t.Fatalf("after the first failure: try again at %v, %d tries again", v.RetryAt, v.Retries)
	}
	tryAgain(t, mt)
	if v := failRun(m, mt, StateError, 300); v.RetryAt.IsZero() || v.Retries != 0 {
		t.Fatalf("a try again got further and then failed: try again at %v, %d tries again; want another, counted afresh", v.RetryAt, v.Retries)
	}
	tryAgain(t, mt)
	if v := failRun(m, mt, StateError, 300); !v.RetryAt.IsZero() || v.Retries != 1 {
		t.Fatalf("a try again got no further: try again at %v, %d tries again; want it given up on after 1", v.RetryAt, v.Retries)
	}
}

// Keep trying never gives up, and an action armed waits; stopping the tries
// again by hand gives the download up, and the action goes ahead.
func TestKeepTryingWaitsUntilTheTriesAreStopped(t *testing.T) {
	m, pc, n := managerWithFakePC(t, 2)
	onFailure(t, m, "forever")
	good, bad := taskIn(m, "good", StateRunning), taskIn(m, "bad", StateRunning)
	arm(t, m, "shutdown")
	failRun(m, bad, StateNeedsRefresh, 0)
	for i := 0; i < 20; i++ {
		tryAgain(t, bad)
		if v := failRun(m, bad, StateNeedsRefresh, 0); v.RetryAt.IsZero() || v.Retries != i+1 {
			t.Fatalf("after %d tries again: try again at %v, %d counted", i+1, v.RetryAt, v.Retries)
		}
	}
	end(m, good, StateDone)
	nothingHappens(t, pc, "a download is still being tried again")
	waitFor(t, 2*time.Second, "the notices", func() bool { return len(n.withTitle("Download link expired")) >= 1 })
	time.Sleep(100 * time.Millisecond)
	if got := n.withTitle("Download link expired"); len(got) != 1 {
		t.Errorf("notices = %q, want one for the failure and none for the tries again", got)
	}

	if !m.Pause("bad") {
		t.Fatal("pause refused")
	}
	happensOnce(t, pc, "shutdown in 1m0s")
	if v, _ := findTask(m, "bad"); v.State != StateNeedsRefresh || !v.RetryAt.IsZero() {
		t.Errorf("stopped, the download is %s and to be tried again at %v", v.State, v.RetryAt)
	}
}

// Stopping the tries again is the user at the machine, which finishes nothing:
// with nothing finished since the action was armed, it does not go ahead.
func TestStoppingTheTriesAgainFinishesNothing(t *testing.T) {
	m, pc, _ := managerWithFakePC(t, 2)
	onFailure(t, m, "forever")
	only := taskIn(m, "only", StateRunning)
	arm(t, m, "shutdown")
	failRun(m, only, StateError, 0)
	m.Pause("only")
	nothingHappens(t, pc, "nothing finished since it was armed")
}

// Choosing an action and Don't try again in the same save finishes nothing
// either, though it ends the wait for the downloads that were to be tried.
func TestChoosingAnActionAndDontTryAgainTogetherSetsNothingOff(t *testing.T) {
	m, pc, _ := managerWithFakePC(t, 2)
	only := taskIn(m, "only", StateRunning)
	failRun(m, only, StateError, 0)
	act, policy := "shutdown", "give_up"
	if err := m.UpdateSettings(settingsUpdate{AfterAll: &act, OnFailure: &policy}); err != nil {
		t.Fatal(err)
	}
	nothingHappens(t, pc, "nothing finished since the action was chosen")
	if v, _ := findTask(m, "only"); !v.RetryAt.IsZero() {
		t.Errorf("under Don't try again the download is still to be tried at %v", v.RetryAt)
	}
}

func TestPauseAllStopsTheTriesAgainAndSetsNothingOff(t *testing.T) {
	m, pc, _ := managerWithFakePC(t, 2)
	f, done := taskIn(m, "f", StateRunning), taskIn(m, "done", StateRunning)
	arm(t, m, "sleep")
	failRun(m, f, StateError, 0)
	end(m, done, StateDone) // something finished: only f holds the action back
	nothingHappens(t, pc, "a download is to be tried again")
	m.PauseAll()
	if v, _ := findTask(m, "f"); v.State != StateError || !v.RetryAt.IsZero() {
		t.Fatalf("after Pause all the download is %s and to be tried again at %v", v.State, v.RetryAt)
	}
	nothingHappens(t, pc, "Pause all is not finishing")
	m.retryTick(time.Now().Add(time.Hour))
	if v, _ := findTask(m, "f"); v.State != StateError {
		t.Errorf("tried again after Pause all: %s", v.State)
	}
}

func TestAskingForANewLinkStopsTheTriesAgain(t *testing.T) {
	m, _, _ := managerWithFakePC(t, 2)
	m.openURL = nil
	mt := taskIn(m, "x", StateRunning)
	mt.mu.Lock()
	mt.view.Path, mt.view.Size = filepath.Join(t.TempDir(), "x.bin"), 100
	mt.mu.Unlock()
	failRun(m, mt, StateNeedsRefresh, 50)
	if _, err := m.RequestRefresh("x"); err != nil {
		t.Fatal(err)
	}
	m.retryTick(time.Now().Add(time.Hour))
	if v, _ := findTask(m, "x"); v.State != StateAwaitingRefresh || !v.RetryAt.IsZero() {
		t.Errorf("waiting for a new link, the download is %s and to be tried again at %v", v.State, v.RetryAt)
	}
}

// ---------- the choice ----------

func TestChangingTheChoiceMovesTheTriesAgain(t *testing.T) {
	m, pc, _ := managerWithFakePC(t, 2)
	if err := m.UpdateSettings(settingsInts(5, 2)); err != nil {
		t.Fatal(err)
	}
	arm(t, m, "sleep")
	waiting, gaveUp, fine := taskIn(m, "waiting", StateRunning), taskIn(m, "gaveup", StateRunning), taskIn(m, "fine", StateRunning)
	failRun(m, waiting, StateError, 0)
	tryAgain(t, waiting)
	failRun(m, waiting, StateError, 0) // counted 1 of 2
	failRun(m, gaveUp, StateNeedsRefresh, 0)
	tryAgain(t, gaveUp)
	failRun(m, gaveUp, StateNeedsRefresh, 0)
	tryAgain(t, gaveUp)
	if v := failRun(m, gaveUp, StateNeedsRefresh, 0); !v.RetryAt.IsZero() {
		t.Fatal("the download was not given up on")
	}
	end(m, fine, StateDone)
	nothingHappens(t, pc, "a download is to be tried again")

	// in checks which are to be tried again, and after how long.
	in := func(d time.Duration, ids ...string) {
		t.Helper()
		for _, id := range []string{"waiting", "gaveup", "fine"} {
			v, _ := findTask(m, id)
			want := false
			for _, w := range ids {
				want = want || w == id
			}
			if !want {
				if !v.RetryAt.IsZero() {
					t.Errorf("%s is to be tried again at %v", id, v.RetryAt)
				}
				continue
			}
			if left := time.Until(v.RetryAt); left > d || left < d-5*time.Second || v.Retries != 0 {
				t.Errorf("%s is tried again in %v, counted %d; want in %v, counted afresh", id, left, v.Retries, d)
			}
		}
	}
	// Another choice that tries again moves only what was to be tried
	// anyway: what was given up on stays given up on.
	onFailure(t, m, "forever")
	in(5*time.Minute, "waiting")
	if err := m.UpdateSettings(settingsInts(10, 1)); err != nil {
		t.Fatal(err)
	}
	in(10*time.Minute, "waiting")

	// Saving the dialog with the choice as it was changes nothing.
	was, _ := findTask(m, "waiting")
	time.Sleep(20 * time.Millisecond)
	forever := "forever"
	if err := m.UpdateSettings(settingsUpdate{OnFailure: &forever, RetryEvery: ptrInt(10), RetryTimes: ptrInt(1)}); err != nil {
		t.Fatal(err)
	}
	if now, _ := findTask(m, "waiting"); !now.RetryAt.Equal(was.RetryAt) {
		t.Errorf("saving the same choice moved the try again from %v to %v", was.RetryAt, now.RetryAt)
	}

	// Don't try again ends the wait, and the action goes ahead for what
	// finished since it was armed.
	onFailure(t, m, "give_up")
	happensOnce(t, pc, "sleep")
	in(0)

	// Turning tries again back on tries every failed download.
	onFailure(t, m, "retry")
	in(10*time.Minute, "waiting", "gaveup")
}

func ptrInt(n int) *int { return &n }

func TestTheChoiceForFailuresIsCheckedAndKept(t *testing.T) {
	store := filepath.Join(t.TempDir(), "tasks.json")
	m := NewManager(t.TempDir(), 2)
	m.store = store
	if p, e, n := m.RetryChoice(); p != "retry" || e != 5 || n != 3 {
		t.Fatalf("default = %s every %d, %d times; want retry every 5, 3 times", p, e, n)
	}
	str := func(s string) *string { return &s }
	for _, bad := range []settingsUpdate{
		{OnFailure: str("explode")}, {OnFailure: str("")}, {RetryEvery: ptrInt(0)}, {RetryEvery: ptrInt(24*60 + 1)},
		{RetryTimes: ptrInt(0)}, {RetryTimes: ptrInt(101)}, {KeepAwake: new(bool), RetryTimes: ptrInt(-1)},
	} {
		if err := m.UpdateSettings(bad); err == nil {
			t.Errorf("%+v was accepted", bad)
		}
	}
	if p, e, n := m.RetryChoice(); p != "retry" || e != 5 || n != 3 || !m.KeepAwake() {
		t.Fatalf("refused requests changed the settings: %s every %d, %d times, keep awake %v", p, e, n, m.KeepAwake())
	}

	if err := m.UpdateSettings(settingsUpdate{OnFailure: str("forever"), RetryEvery: ptrInt(30), RetryTimes: ptrInt(5)}); err != nil {
		t.Fatal(err)
	}
	view := m.settingsView()
	if view["on_failure"] != "forever" || view["retry_every"] != 30 || view["retry_times"] != 5 {
		t.Errorf("the dialog is told %v, %v, %v", view["on_failure"], view["retry_every"], view["retry_times"])
	}
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	again := NewManager(t.TempDir(), 2)
	again.store = store
	if err := again.load(); err != nil {
		t.Fatal(err)
	}
	if p, e, n := again.RetryChoice(); p != "forever" || e != 30 || n != 5 {
		t.Errorf("after a restart: %s every %d, %d times", p, e, n)
	}
	if c := again.failing.Load(); c.policy != failForever || c.every != 30*time.Minute || c.times != 5 {
		t.Errorf("after a restart failing runs read %+v", *c)
	}

	// A list from before there was a choice, or with nonsense in it, gets the
	// defaults.
	for name, settings := range map[string]string{
		"older":    `{"keep_awake":true}`,
		"nonsense": `{"keep_awake":true,"on_failure":"explode","retry_every":-3,"retry_times":1000}`,
	} {
		if err := os.WriteFile(store, []byte(`{"version":1,"limit":2,"tasks":[],"settings":`+settings+`}`), 0o600); err != nil {
			t.Fatal(err)
		}
		m := NewManager(t.TempDir(), 2)
		m.store = store
		if err := m.load(); err != nil {
			t.Fatal(err)
		}
		if p, e, n := m.RetryChoice(); p != "retry" || e != 5 || n != 3 {
			t.Errorf("%s list: %s every %d, %d times; want the defaults", name, p, e, n)
		}
	}
}

// Nothing starts by itself after a restart: a failed download waits for the
// user, as one that was running waits paused.
func TestARestartLeavesFailedDownloadsToTheUser(t *testing.T) {
	store := filepath.Join(t.TempDir(), "tasks.json")
	m, _, _ := managerWithFakePC(t, 2)
	m.store = store
	f := taskIn(m, "f", StateRunning)
	failRun(m, f, StateError, 0)
	tryAgain(t, f)
	if v := failRun(m, f, StateError, 0); v.RetryAt.IsZero() || v.Retries != 1 {
		t.Fatalf("before the restart: try again at %v, %d counted", v.RetryAt, v.Retries)
	}
	if err := m.save(); err != nil {
		t.Fatal(err)
	}

	again, _, _ := managerWithFakePC(t, 2)
	again.store = store
	if err := again.load(); err != nil {
		t.Fatal(err)
	}
	if v, _ := findTask(again, "f"); v.State != StateError || !v.RetryAt.IsZero() || v.Retries != 0 {
		t.Errorf("after a restart the download is %s, to be tried again at %v, %d counted", v.State, v.RetryAt, v.Retries)
	}
}

// Starting the list starts by hand a try again still waiting for its turn,
// which then counts afresh.
func TestStartingTheListCountsATryAgainWaitingForItsTurnAfresh(t *testing.T) {
	m, _, _ := managerWithFakePC(t, 2)
	mt := taskIn(m, "x", StateQueued)
	mt.mu.Lock()
	mt.req.Later, mt.view.Later = true, true
	mt.autoGen, mt.view.Retries = mt.gen, 2
	mt.mu.Unlock()
	if !m.startLater("x") {
		t.Fatal("the list did not start it")
	}
	mt.mu.Lock()
	auto, n := mt.autoGen == mt.gen, mt.view.Retries
	mt.mu.Unlock()
	if auto || n != 0 {
		t.Errorf("started from the list, the run still counts as a try again (%v), %d counted", auto, n)
	}
}
