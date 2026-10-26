package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The download list holds downloads that are wanted later rather than now, the
// way IDM's "Download Later" does. A download added to it is tried at once: it
// runs until it has fetched a little or a short while has passed, which shows
// that the link works, how large the file is and how fast it comes, and then it
// stops and waits on the list. Start (one, or the whole list at once, by hand or
// at a time set in advance) lets it run to the end like any other download.
//
// A task is on the list while its request says Later. The try runs outside the
// queue: a list built while long downloads fill every slot would otherwise not
// be tried until they had finished, hours later, and a link found broken then
// is a link found broken too late. Tries take a lane of their own instead, a
// few at a time, which costs little because each is short.

// StateListed is a task on the download list that has been tried and waits to
// be started.
const StateListed TaskState = "listed"

var (
	// tryBytes is how much a try fetches before the task waits on the list.
	tryBytes int64 = 8 << 20
	// tryFor is the longest a try runs once data could start to flow: from
	// when it got its slot, or for a torrent from when it knew its files. A
	// slow link stops here having fetched less, and one that sent nothing says
	// so by its empty progress.
	tryFor = 30 * time.Second
	// tryMetaFor is the longest a torrent's try waits for the list of its
	// files. Peers that never answer would otherwise hold a lane of tries for
	// the five minutes the torrent itself waits.
	tryMetaFor = 2 * time.Minute
	// tryTick is how often a try is looked at. Variables so tests can shorten them.
	tryTick = 250 * time.Millisecond
)

// tryLanes is how many tries run at once, beside the downloads.
const tryLanes = 2

// setLaterLocked puts the task on the list or takes it off. One taken off
// before it is done was started from the list, and the list goes on showing it
// (FromList) until it is: a list that empties the moment it is started looks
// as if nothing happened, when its downloads may only be waiting their turn in
// the queue. Callers hold t.mu.
func (t *managedTask) setLaterLocked(on bool) {
	if t.req.Later && !on && t.view.State != StateDone {
		t.view.FromList = true
	}
	t.req.Later = on
	t.view.Later = on
}

// watchTry ends a run that is only a try once it has fetched tryBytes or run
// for tryFor, by calling stop, and marks the run as one that stopped for that
// reason so that it ends on the list and not paused (see finishRun). A task
// started for real is stopped by startLater instead, so it gives up watching.
func (m *Manager) watchTry(ctx context.Context, mt *managedTask, gen int, stop context.CancelFunc) {
	tick := time.NewTicker(tryTick)
	defer tick.Stop()
	slot := time.Now()
	var begun time.Time
	var from int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		mt.mu.Lock()
		if mt.gen != gen || !mt.req.Later {
			mt.mu.Unlock()
			return
		}
		// A torrent fetches the list of its files from peers before any of
		// them, which can take minutes and is not what the try is timing.
		ready := mt.view.Kind != "bt" || mt.view.Size > 0
		if ready && begun.IsZero() {
			begun, from = time.Now(), mt.view.Received
		}
		// What is counted can start again from nothing: a server that will not
		// resume, or a video whose sound comes after its picture.
		from = min(from, mt.view.Received)
		held := false
		if ready {
			got, took := mt.view.Received-from, time.Since(begun)
			if got >= tryBytes || took >= tryFor {
				held = true
				if secs := took.Seconds(); secs > 0 {
					mt.view.TrySpeed = int64(float64(got) / secs)
				}
			}
		} else if time.Since(slot) >= tryMetaFor {
			held = true
		}
		if held {
			mt.tryHeld = gen
		}
		mt.mu.Unlock()
		if held {
			stop()
			return
		}
	}
}

// stoppedLocked is what a run that was called off ends as, decided under the
// same lock that writes it, so that a start from the list cannot fall between
// the two. A try that has done its part waits on the list; anything else was
// paused. A try taken off the list as it stopped has to run again, now in the
// queue, which again asks for. Callers hold t.mu.
func (t *managedTask) stoppedLocked(gen int) (st TaskState, again bool) {
	if t.tryHeld != gen {
		return StatePaused, false
	}
	if t.req.Later {
		return StateListed, false
	}
	return StatePaused, true
}

// StartList starts everything on the download list, oldest first, so that it
// joins the queue in the order it was put on the list. It returns how many it
// started.
func (m *Manager) StartList() int {
	n := 0
	for _, v := range m.list(true) {
		if v.Later && v.State != StateDone && m.startLater(v.ID) {
			n++
		}
	}
	return n
}

// startLater takes one task off the list and lets it download in full. One
// that is waiting on the list, or stopped, joins the queue, and so does one
// still waiting for its try. One being tried is stopped and joins the queue
// too: let run on in the lane of tries, it would get round the limit on
// downloads at once, and hold up the tries behind it until it finished.
func (m *Manager) startLater(id string) bool {
	mt := m.get(id)
	if mt == nil {
		return false
	}
	mt.mu.Lock()
	if !mt.req.Later {
		mt.mu.Unlock()
		return false
	}
	switch mt.view.State {
	case StateQueued:
		mt.setLaterLocked(false)
		mt.mu.Unlock()
		m.dirty.Store(true)
		m.mu.Lock()
		m.broadcastLocked() // it now waits in the other line
		m.mu.Unlock()
		return true
	case StateRunning:
		mt.setLaterLocked(false)
		mt.tryHeld = mt.gen // finishRun then starts it again, in the queue
		cancel := mt.cancel
		mt.mu.Unlock()
		m.dirty.Store(true)
		if cancel != nil {
			cancel()
		}
		return true
	case StateNeedsRefresh, StateAwaitingRefresh:
		// Run again on the link that expired it would only expire again, and
		// one waiting for a new link would stop waiting. It leaves the list
		// and waits for its link where it is.
		mt.setLaterLocked(false)
		mt.mu.Unlock()
		m.dirty.Store(true)
		return false
	case StateDone:
		mt.setLaterLocked(false)
		mt.mu.Unlock()
		m.dirty.Store(true)
		return false
	}
	mt.mu.Unlock()
	return m.Resume(id)
}

// nextTryLocked is the oldest task waiting for a try, as nextInLineLocked is
// for the queue. Callers hold m.mu.
func (m *Manager) nextTryLocked() *managedTask {
	for _, id := range m.order {
		t := m.tasks[id]
		if t == nil {
			continue
		}
		t.mu.Lock()
		eligible := t.view.State == StateQueued && t.req.Later && !t.matching
		t.mu.Unlock()
		if eligible {
			return t
		}
	}
	return nil
}

// ---------- starting the list at a set time ----------

// listClockEvery is how often the time to start the list is checked.
var listClockEvery = 15 * time.Second

// listLate is how late a start may still be made. A computer that slept or was
// off through the time set does not start the list at some other hour when it
// wakes: that is not what was asked for.
const listLate = 30 * time.Minute

// checkListAt validates the time of day the list is to start at, "15:04", and
// returns it as written; empty turns starting at a time off.
func checkListAt(raw string) (string, error) {
	at := strings.TrimSpace(raw)
	if at == "" {
		return "", nil
	}
	t, err := time.Parse("15:04", at)
	if err != nil {
		return "", fmt.Errorf("the time to start the list must be hours and minutes, like 02:30, not %q", raw)
	}
	return t.Format("15:04"), nil
}

// nextListStart is the next time after now that the clock reads at, today or
// tomorrow, in local time.
func nextListStart(at string, now time.Time) time.Time {
	t, err := time.Parse("15:04", at)
	if err != nil {
		return time.Time{}
	}
	next := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// planListLocked works out when the list starts next from the settings.
// Callers hold m.mu.
func (m *Manager) planListLocked(now time.Time) {
	if m.settings.ListAt == "" {
		m.listNext = time.Time{}
		return
	}
	m.listNext = nextListStart(m.settings.ListAt, now)
}

// ListNext is when the list starts next by the clock, zero when it does not.
func (m *Manager) ListNext() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listNext
}

// listClock starts the list when its time comes, until stop is closed.
func (m *Manager) listClock(stop <-chan struct{}) {
	tick := time.NewTicker(listClockEvery)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			m.listTick(time.Now())
		}
	}
}

// listTick is one look at the clock: if the time set has come, the list is
// started, and the next time is set, tomorrow's for a daily start and none for
// a start made once.
func (m *Manager) listTick(now time.Time) {
	m.mu.Lock()
	due := m.listNext
	if due.IsZero() || now.Before(due) {
		m.mu.Unlock()
		return
	}
	late := now.Sub(due)
	if m.settings.ListDaily {
		m.listNext = nextListStart(m.settings.ListAt, now)
	} else {
		m.settings.ListAt, m.listNext = "", time.Time{}
	}
	m.mu.Unlock()
	m.dirty.Store(true)
	if late > listLate {
		return
	}
	if n := m.StartList(); n > 0 && m.announce != nil && m.notifyOn.Load() {
		m.announce(m.tr("notify.listGo"), m.tr("notify.listGoN", "n", n), "app")
	}
}
