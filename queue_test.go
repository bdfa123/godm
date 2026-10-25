package main

import (
	"context"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"
)

// A download that waits for a slot is paused and resumed again at once, as a
// script does, or Pause all followed straight by Resume all. The run that was
// waiting learns of the pause some time later, and by then the task is queued
// again for the run that replaces it. Whatever the old run does on its way out
// has to leave that alone.
//
// waitingInAFullQueue returns such a download with the generation of the run
// that waits. The queue is full only on paper, so that making room is all it
// takes to let the download through.
func waitingInAFullQueue(t *testing.T) (*Manager, string, *managedTask, int) {
	t.Helper()
	src := httptest.NewServer(&rangedHandler{payload: makePayload(64 << 10)})
	t.Cleanup(src.Close)
	m := NewManager(t.TempDir(), 1)
	m.announce = nil
	m.mu.Lock()
	m.running = m.limit
	m.mu.Unlock()
	id, err := m.Add(jobRequest{URL: src.URL + "/small.bin", Connections: 1})
	if err != nil {
		t.Fatal(err)
	}
	mt := m.get(id)
	var waiting int
	waitFor(t, 10*time.Second, "the run to be waiting for a slot", func() bool {
		mt.mu.Lock()
		defer mt.mu.Unlock()
		waiting = mt.gen
		return mt.waitingSlot
	})
	return m, id, mt, waiting
}

// pauseThenResume does both with nothing able to run in between or straight
// after: on one processor the goroutine Resume starts does not get going
// until the caller waits for something, and neither does the run the pause
// called off. That holds the task at the point where the resume used to be
// lost, queued again with the new run not yet begun, for the caller to play
// the old run's part by hand. It returns what puts the processors back.
func pauseThenResume(t *testing.T, m *Manager, id string) (restore func()) {
	t.Helper()
	procs := runtime.GOMAXPROCS(1)
	restore = func() { runtime.GOMAXPROCS(procs) }
	m.Pause(id)
	if !m.Resume(id) {
		restore()
		t.Fatal("the resume was refused")
	}
	return restore
}

func TestAResumeStraightAfterAPauseInTheQueueIsNotLost(t *testing.T) {
	m, id, mt, waiting := waitingInAFullQueue(t)

	restore := pauseThenResume(t, m, id)
	// What the waiting run does when it finds its wait called off.
	m.finishRun(mt, waiting, StatePaused, "", "")
	st := mt.snapshot().State
	restore()
	if st != StateQueued {
		t.Fatalf("the last thing asked for was a resume, and it was accepted, but the download is %s: "+
			"the run the pause called off wrote its own ending over it", st)
	}

	m.SetLimit(2)
	waitFor(t, 20*time.Second, "the resumed download to run once there is room", stateIs(m, id, StateDone))
}

// The old run can also come round to a free slot before it notices that it
// was called off. It must not take it: it would start downloading on a
// cancelled context, stop at once and leave the task paused, with the run that
// was to replace it turned away because the task was already running.
func TestARunCalledOffInTheQueueDoesNotTakeTheSlot(t *testing.T) {
	m, id, mt, waiting := waitingInAFullQueue(t)

	restore := pauseThenResume(t, m, id)
	m.mu.Lock()
	m.running = 0 // room at last
	m.mu.Unlock()
	calledOff, cancel := context.WithCancel(context.Background())
	cancel()
	_, took := m.acquire(calledOff, mt, waiting)
	st := mt.snapshot().State
	restore()
	if took {
		t.Fatalf("the run the pause called off took the slot and set the download to %s", st)
	}

	m.mu.Lock()
	m.broadcastLocked()
	m.mu.Unlock()
	waitFor(t, 20*time.Second, "the resumed download to run", stateIs(m, id, StateDone))
}
