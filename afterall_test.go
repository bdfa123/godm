package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePC stands in for the computer. Nothing here can touch the real one: it
// records what it was asked to do.
type fakePC struct {
	awake fakeAwake

	mu      sync.Mutex
	actions []string
	fail    error
	onAct   func() // runs inside the call, as the real thing would be
	reason  string // what the last shutdown said it was for
}

func (f *fakePC) ops() powerOps {
	return powerOps{
		keepAwake: f.awake.set,
		sleep: func() error {
			f.record("sleep")
			return f.err()
		},
		shutdown: func(d time.Duration, reason string) error {
			f.mu.Lock()
			f.reason = reason
			f.mu.Unlock()
			f.record("shutdown in " + d.String())
			return f.err()
		},
	}
}

func (f *fakePC) record(a string) {
	f.mu.Lock()
	f.actions = append(f.actions, a)
	hook := f.onAct
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
}

func (f *fakePC) err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fail
}

func (f *fakePC) did() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.actions...)
}

// notices collects what the manager tells the user.
type notices struct {
	mu    sync.Mutex
	items []string
}

func (n *notices) add(title, text, open string) {
	n.mu.Lock()
	n.items = append(n.items, title+" | "+strings.ReplaceAll(text, "\n", " "))
	n.mu.Unlock()
}

func (n *notices) all() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.items...)
}

// withTitle returns the notices whose title starts with prefix.
func (n *notices) withTitle(prefix string) []string {
	var out []string
	for _, s := range n.all() {
		if strings.HasPrefix(s, prefix) {
			out = append(out, s)
		}
	}
	return out
}

func managerWithFakePC(t *testing.T, parallel int) (*Manager, *fakePC, *notices) {
	t.Helper()
	m := NewManager(t.TempDir(), parallel)
	pc, n := &fakePC{}, &notices{}
	m.announce = n.add
	m.SetPower(pc.ops())
	t.Cleanup(func() { m.SetPower(powerOps{}) })
	return m, pc, n
}

// arm sets what to do when the downloads finish, the way the dialog does.
func arm(t *testing.T, m *Manager, act string) {
	t.Helper()
	if err := m.UpdateSettings(settingsUpdate{AfterAll: &act}); err != nil {
		t.Fatal(err)
	}
}

// taskIn puts a task straight into the list in the given state, so a test can
// end its run whichever way it likes without a download behind it.
func taskIn(m *Manager, id string, st TaskState) *managedTask {
	mt := &managedTask{
		view:   TaskView{ID: id, Filename: id + ".bin", State: st, AddedAt: time.Now()},
		outDir: os.TempDir(),
	}
	mt.gen = 1
	m.mu.Lock()
	m.tasks[id] = mt
	m.order = append(m.order, id)
	m.mu.Unlock()
	return mt
}

// end finishes a task's run in the given state.
func end(m *Manager, mt *managedTask, st TaskState) {
	m.finishRun(mt, 1, st, "", "")
}

// nothingHappens checks that no action runs, allowing a moment for one that
// wrongly would.
func nothingHappens(t *testing.T, pc *fakePC, why string) {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
	if got := pc.did(); len(got) != 0 {
		t.Fatalf("%s, but the computer was asked to %v", why, got)
	}
}

func happensOnce(t *testing.T, pc *fakePC, want string) {
	t.Helper()
	waitFor(t, 5*time.Second, want, func() bool { return len(pc.did()) > 0 })
	time.Sleep(100 * time.Millisecond)
	if got := pc.did(); !reflect.DeepEqual(got, []string{want}) {
		t.Fatalf("the computer was asked to %v, want exactly [%s]", got, want)
	}
}

// ---------- real downloads ----------

func TestAfterAllRunsOnceWhenTheLastDownloadFinishes(t *testing.T) {
	m, pc, _ := managerWithFakePC(t, 2)
	quick := newTestServer(t, &slowServer{payload: makePayload(16 << 10), chunk: 16 << 10, delay: time.Millisecond})
	arm(t, m, "sleep")

	fast, _ := m.Add(jobRequest{URL: quick.URL + "/quick.bin", Connections: 1})
	slow, _ := m.Add(jobRequest{URL: slowFile(t), Filename: "slow.bin", Connections: 1})

	waitFor(t, 20*time.Second, "the quick download", stateIs(m, fast, StateDone))
	if v, _ := findTask(m, slow); v.State == StateDone {
		t.Fatal("the slow download was over before the quick one was looked at; the test proves nothing")
	}
	nothingHappens(t, pc, "a download is still running")

	waitFor(t, 20*time.Second, "the slow download", stateIs(m, slow, StateDone))
	happensOnce(t, pc, "sleep")
	if got := m.AfterAll(); got != "nothing" {
		t.Errorf("after running, the choice is %q, want it back at nothing", got)
	}

	// It was a one-shot: the next download finishing must not set it off again.
	again, _ := m.Add(jobRequest{URL: quick.URL + "/again.bin", Connections: 1})
	waitFor(t, 20*time.Second, "another download", stateIs(m, again, StateDone))
	time.Sleep(150 * time.Millisecond)
	if got := pc.did(); len(got) != 1 {
		t.Errorf("the action ran again: %v", got)
	}
}

func TestAfterAllWaitsForDownloadsStillInTheQueue(t *testing.T) {
	m, pc, _ := managerWithFakePC(t, 1) // one at a time, so two of the three are queued
	arm(t, m, "sleep")

	var statesWhenRun []TaskState
	pc.onAct = func() {
		for _, v := range m.List() {
			statesWhenRun = append(statesWhenRun, v.State)
		}
	}
	srv := newTestServer(t, &slowServer{payload: makePayload(64 << 10), chunk: 16 << 10, delay: 40 * time.Millisecond})
	for i := 1; i <= 3; i++ {
		if _, err := m.Add(jobRequest{URL: fmt.Sprintf("%s/q%d.bin", srv.URL, i), Connections: 1}); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, 30*time.Second, "the action", func() bool { return len(pc.did()) > 0 })
	time.Sleep(100 * time.Millisecond)

	want := []TaskState{StateDone, StateDone, StateDone}
	if !reflect.DeepEqual(statesWhenRun, want) {
		t.Errorf("when the action ran the downloads were %v, want %v", statesWhenRun, want)
	}
	if got := pc.did(); len(got) != 1 {
		t.Errorf("actions = %v, want one", got)
	}
}

// ---------- which states count ----------

func TestAfterAllNeedsADownloadToFinishAfterItIsSet(t *testing.T) {
	m, pc, _ := managerWithFakePC(t, 2)
	earlier := taskIn(m, "earlier", StateRunning)
	end(m, earlier, StateDone) // before it is armed

	arm(t, m, "sleep")
	m.checkAfterAll(false)
	nothingHappens(t, pc, "nothing has finished since it was armed")

	later := taskIn(m, "later", StateRunning)
	end(m, later, StateDone)
	happensOnce(t, pc, "sleep")
}

func TestAFailedDownloadNeverSetsItOff(t *testing.T) {
	for _, st := range []TaskState{StateError, StateNeedsRefresh} {
		t.Run(string(st), func(t *testing.T) {
			m, pc, _ := managerWithFakePC(t, 2)
			arm(t, m, "shutdown")
			only := taskIn(m, "only", StateRunning)
			end(m, only, st)
			nothingHappens(t, pc, "the only download failed, which is not finishing")
			if got := m.AfterAll(); got != "shutdown" {
				t.Errorf("the choice was %q after a failure, want it still armed", got)
			}
		})
	}
}

func TestFailedDownloadsDoNotHoldItBack(t *testing.T) {
	for _, st := range []TaskState{StateError, StateNeedsRefresh} {
		t.Run(string(st)+" first", func(t *testing.T) {
			m, pc, _ := managerWithFakePC(t, 2)
			good, bad := taskIn(m, "good", StateRunning), taskIn(m, "bad", StateRunning)
			arm(t, m, "sleep")
			end(m, bad, st)
			nothingHappens(t, pc, "the good download is still running")
			end(m, good, StateDone)
			happensOnce(t, pc, "sleep")
		})
		t.Run(string(st)+" last", func(t *testing.T) {
			m, pc, _ := managerWithFakePC(t, 2)
			good, bad := taskIn(m, "good", StateRunning), taskIn(m, "bad", StateRunning)
			arm(t, m, "sleep")
			end(m, good, StateDone)
			nothingHappens(t, pc, "the other download is still running")
			end(m, bad, st)
			happensOnce(t, pc, "sleep")
		})
	}
}

func TestPausedDownloadsDoNotHoldItBack(t *testing.T) {
	m, pc, _ := managerWithFakePC(t, 2)
	taskIn(m, "parked", StatePaused)
	running := taskIn(m, "running", StateRunning)
	arm(t, m, "sleep")
	end(m, running, StateDone)
	happensOnce(t, pc, "sleep")
}

func TestPausingTheLastDownloadDoesNotSetItOff(t *testing.T) {
	m, pc, _ := managerWithFakePC(t, 2)
	first, second := taskIn(m, "first", StateRunning), taskIn(m, "second", StateRunning)
	arm(t, m, "shutdown")
	end(m, first, StateDone)
	end(m, second, StatePaused) // the user stopped it; they are at the machine
	nothingHappens(t, pc, "the last download was paused, not finished")
}

func TestQueuedDownloadsHoldItBack(t *testing.T) {
	m, pc, _ := managerWithFakePC(t, 1)
	running, queued := taskIn(m, "running", StateRunning), taskIn(m, "queued", StateQueued)
	arm(t, m, "sleep")
	end(m, running, StateDone)
	nothingHappens(t, pc, "another download is waiting its turn")
	end(m, queued, StateDone)
	happensOnce(t, pc, "sleep")
}

// A download waiting for a fresh link holds the action back because the user is
// in the middle of fixing it, but not for ever.
func TestWaitingForANewLinkHoldsItBackOnlyUntilTheWaitEnds(t *testing.T) {
	m, pc, _ := managerWithFakePC(t, 2)
	running := taskIn(m, "running", StateRunning)
	waiting := taskIn(m, "waiting", StateAwaitingRefresh)
	waiting.view.RefreshUntil = time.Now().Add(600 * time.Millisecond)
	arm(t, m, "sleep")

	start := time.Now()
	end(m, running, StateDone)
	nothingHappens(t, pc, "a download is still waiting for its new link")

	// Nobody touches anything in the meantime: the end of the wait is what
	// brings the action on, with no download finishing to prompt it.
	waitFor(t, 5*time.Second, "the action once the wait ran out", func() bool { return len(pc.did()) > 0 })
	if took := time.Since(start); took < 500*time.Millisecond {
		t.Errorf("ran after %v, before the wait for the new link was over", took)
	}
	happensOnce(t, pc, "sleep")
}

func TestAWaitThatIsAlreadyOverDoesNotHoldItBack(t *testing.T) {
	m, pc, _ := managerWithFakePC(t, 2)
	running := taskIn(m, "running", StateRunning)
	stale := taskIn(m, "stale", StateAwaitingRefresh)
	stale.view.RefreshUntil = time.Now().Add(-time.Minute) // not yet tidied away by anything
	arm(t, m, "sleep")
	end(m, running, StateDone)
	happensOnce(t, pc, "sleep")
}

// ---------- the choice itself ----------

func TestChoosingTheSameThingAgainKeepsCounting(t *testing.T) {
	m, pc, _ := managerWithFakePC(t, 2)
	a, b := taskIn(m, "a", StateRunning), taskIn(m, "b", StateRunning)
	arm(t, m, "sleep")
	end(m, a, StateDone)
	arm(t, m, "sleep") // the same again: a already finished since it was armed
	end(m, b, StateError)
	happensOnce(t, pc, "sleep")
}

func TestChoosingSomethingElseStartsCountingAfresh(t *testing.T) {
	m, pc, _ := managerWithFakePC(t, 2)
	a, b := taskIn(m, "a", StateRunning), taskIn(m, "b", StateRunning)
	arm(t, m, "sleep")
	end(m, a, StateDone)
	arm(t, m, "shutdown") // a new decision: a finished before it
	end(m, b, StateError)
	nothingHappens(t, pc, "nothing finished since shutdown was chosen")
}

func TestChoosingNothingDisarms(t *testing.T) {
	m, pc, _ := managerWithFakePC(t, 2)
	only := taskIn(m, "only", StateRunning)
	arm(t, m, "shutdown")
	arm(t, m, "nothing")
	end(m, only, StateDone)
	nothingHappens(t, pc, "it was switched back to nothing")
}

func TestAfterAllRefusesWhatItCannotDo(t *testing.T) {
	m := NewManager(t.TempDir(), 2)
	m.SetPower(powerOps{shutdown: func(time.Duration, string) error { return nil }}) // no sleeping here
	t.Cleanup(func() { m.SetPower(powerOps{}) })

	for _, bad := range []string{"hibernate", "", "SHUTDOWN", "sleep"} {
		act := bad
		if err := m.UpdateSettings(settingsUpdate{AfterAll: &act}); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	if got := m.AfterAll(); got != "nothing" {
		t.Errorf("a refused choice left the setting at %q", got)
	}
	if got, want := m.AfterOptions(), []string{"nothing", "shutdown"}; !reflect.DeepEqual(got, want) {
		t.Errorf("options = %v, want %v", got, want)
	}
	// A bad value must not take the rest of the request down with it.
	off, bad := false, "hibernate"
	if err := m.UpdateSettings(settingsUpdate{KeepAwake: &off, AfterAll: &bad}); err == nil {
		t.Fatal("a request with a bad choice was accepted")
	}
	if !m.KeepAwake() {
		t.Error("a refused request still changed keep-awake")
	}
}

// ---------- what the user is told ----------

func TestShutdownWaitsAMinuteAndSaysHowToCancel(t *testing.T) {
	m, pc, n := managerWithFakePC(t, 2)
	m.SetNotifications(false) // this warning is not one the user can switch off
	only := taskIn(m, "only", StateRunning)
	arm(t, m, "shutdown")
	end(m, only, StateDone)

	happensOnce(t, pc, "shutdown in 1m0s")
	waitFor(t, 5*time.Second, "the notice", func() bool { return len(n.withTitle("Shutting down")) > 0 })
	got := n.withTitle("Shutting down")[0]
	if !strings.Contains(got, "60 seconds") || !strings.Contains(got, "shutdown /a") {
		t.Errorf("notice = %q, want it to give the delay and how to cancel", got)
	}
	if got := n.withTitle("Download complete"); len(got) != 0 {
		t.Errorf("notifications are off but %v was shown", got)
	}
}

func TestAShutdownThatCannotBeScheduledSaysSoAndDoesNotClaimOtherwise(t *testing.T) {
	m, pc, n := managerWithFakePC(t, 2)
	pc.fail = errors.New("shutdown.exe: exit status 1190")
	only := taskIn(m, "only", StateRunning)
	arm(t, m, "shutdown")
	end(m, only, StateDone)

	waitFor(t, 5*time.Second, "the notice", func() bool { return len(n.withTitle("Could not shut")) > 0 })
	if got := n.withTitle("Shutting down"); len(got) != 0 {
		t.Errorf("told the user it was shutting down when it was not: %v", got)
	}
	if got := m.AfterAll(); got != "nothing" {
		t.Errorf("choice = %q after the attempt, want it spent", got)
	}
}

func TestASleepThatFailsSaysSo(t *testing.T) {
	m, pc, n := managerWithFakePC(t, 2)
	pc.fail = errors.New("not allowed")
	only := taskIn(m, "only", StateRunning)
	arm(t, m, "sleep")
	end(m, only, StateDone)
	waitFor(t, 5*time.Second, "the notice", func() bool { return len(n.withTitle("Could not put")) > 0 })
}

// ---------- not remembered ----------

func TestAfterAllIsNotKeptAcrossARestart(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "tasks.json")
	m, _, _ := managerWithFakePC(t, 2)
	m.store = store
	arm(t, m, "shutdown")
	if err := m.save(); err != nil {
		t.Fatal(err)
	}

	raw, _ := os.ReadFile(store)
	if strings.Contains(string(raw), "shutdown") || strings.Contains(string(raw), "after") {
		t.Errorf("the armed action was written to disk: %s", raw)
	}
	m2, pc2, _ := managerWithFakePC(t, 2)
	m2.store = store
	if err := m2.load(); err != nil {
		t.Fatal(err)
	}
	if got := m2.AfterAll(); got != "nothing" {
		t.Fatalf("after a restart the choice is %q, want nothing", got)
	}
	only := taskIn(m2, "only", StateRunning)
	end(m2, only, StateDone)
	nothingHappens(t, pc2, "the daemon restarted since it was armed")
}
