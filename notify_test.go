package main

import (
	"context"
	"math"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Notifications are for things the user did not ask for: a download that
// finished, failed, or needs a fresh link. Pausing is their own doing.
func TestNotificationsOnlyForUnattendedOutcomes(t *testing.T) {
	m := NewManager(t.TempDir(), 2)
	var mu sync.Mutex
	var got []string
	m.announce = func(title, text, open string) {
		mu.Lock()
		got = append(got, title+" | "+open+" | "+strings.ReplaceAll(text, "\n", " · "))
		mu.Unlock()
	}
	seen := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}

	mt := &managedTask{
		view: TaskView{
			ID: "x", Filename: "movie.mkv", Size: 4 << 20,
			StartedAt: time.Now().Add(-3 * time.Second),
		},
		outDir: t.TempDir(),
	}
	mt.gen = 1
	m.tasks["x"] = mt
	m.order = append(m.order, "x")

	m.finishRun(mt, 1, StateDone, "", `C:\Users\x\Downloads\movie.mkv`)
	waitFor(t, 2*time.Second, "the completion notification", func() bool { return len(seen()) == 1 })
	if n := seen()[0]; !strings.HasPrefix(n, "Download complete | C:\\Users\\x\\Downloads\\movie.mkv") || !strings.Contains(n, "4.0 MiB") {
		t.Errorf("completion notification = %q", n)
	}

	m.finishRun(mt, 1, StatePaused, "", "")
	time.Sleep(300 * time.Millisecond)
	if len(seen()) != 1 {
		t.Errorf("pausing should stay quiet, got %v", seen())
	}

	m.finishRun(mt, 1, StateNeedsRefresh, "download link expired: server returned 403 Forbidden", "")
	waitFor(t, 2*time.Second, "the expired-link notification", func() bool { return len(seen()) == 2 })
	if n := seen()[1]; !strings.HasPrefix(n, "Download link expired | app") {
		t.Errorf("expiry notification = %q", n)
	}

	m.SetNotifications(false)
	m.finishRun(mt, 1, StateError, "disk full", "")
	time.Sleep(300 * time.Millisecond)
	if len(seen()) != 2 {
		t.Errorf("notifications were switched off, got %v", seen())
	}

	m.SetNotifications(true)
	m.finishRun(mt, 1, StateError, "disk full", "")
	waitFor(t, 2*time.Second, "the failure notification", func() bool { return len(seen()) == 3 })
	if n := seen()[2]; !strings.HasPrefix(n, "Download failed | app") || !strings.Contains(n, "disk full") {
		t.Errorf("failure notification = %q", n)
	}
}

// ---------- how long it took ----------

// "finished in … · avg …" on the card and "… in …" in the notice both divide
// the file's size by the time between started_at and ended_at. That has to be
// the time the download spent downloading, over every run it took: counted
// from the last resume alone, a film paused at 95 % reads as fetched in half a
// minute at a speed the line never had.

// pausedHalfway downloads about half of a three-megabyte file at a megabyte a
// second and pauses it. It returns how long that run had the download going.
func pausedHalfway(t *testing.T, m *Manager) (id string, first time.Duration) {
	t.Helper()
	src := httptest.NewServer(&slowServer{payload: makePayload(3 << 20), chunk: 16 << 10, delay: 30 * time.Millisecond})
	t.Cleanup(src.Close)
	id, err := m.Add(jobRequest{URL: src.URL + "/film.bin", Connections: 2})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the download to start", stateIs(m, id, StateRunning))
	began := time.Now()
	waitFor(t, 20*time.Second, "half of the file", func() bool {
		v, _ := findTask(m, id)
		return v.Received > 1<<20
	})
	m.Pause(id)
	waitFor(t, 10*time.Second, "the pause", stateIs(m, id, StatePaused))
	return id, time.Since(began)
}

// finishedIn resumes the download and checks what it then says it took
// against the two runs the test timed itself.
func finishedIn(t *testing.T, m *Manager, id string, first time.Duration, notices func() []string) {
	t.Helper()
	resumed := time.Now()
	if !m.Resume(id) {
		t.Fatal("resume refused")
	}
	waitFor(t, 30*time.Second, "the download to finish", stateIs(m, id, StateDone))
	second := time.Since(resumed)
	v, _ := findTask(m, id)
	said := v.EndedAt.Sub(v.StartedAt)
	// The runs were timed from outside, a little generously at both ends.
	const slack = 600 * time.Millisecond
	if said < first+second-slack {
		t.Errorf("downloading took %v and then %v, and the task says it finished in %v, "+
			"which makes the card read avg %s/s for a line that gave about 1 MiB/s",
			first.Round(10*time.Millisecond), second.Round(10*time.Millisecond), said.Round(10*time.Millisecond),
			humanBytes(int64(float64(v.Size)/said.Seconds())))
	}
	if said > first+second+slack/2 {
		t.Errorf("downloading took %v and then %v, and the task says it finished in %v: "+
			"time it spent paused was counted", first.Round(10*time.Millisecond),
			second.Round(10*time.Millisecond), said.Round(10*time.Millisecond))
	}
	waitFor(t, 2*time.Second, "the completion notice", func() bool { return len(notices()) == 1 })
	want := span(langEN, int64(math.Round(said.Seconds())))
	if n := notices()[0]; !strings.Contains(n, " in "+want) {
		t.Errorf("the notice says %q, want it to say the download took %s", n, want)
	}
}

func noticesOf(m *Manager) func() []string {
	var mu sync.Mutex
	var got []string
	m.announce = func(title, text, open string) {
		mu.Lock()
		got = append(got, title+" | "+strings.ReplaceAll(text, "\n", " · "))
		mu.Unlock()
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

func TestFinishedInCountsEveryRunOfADownload(t *testing.T) {
	m := NewManager(t.TempDir(), 1)
	setLanguage(t, m, langEN)
	notices := noticesOf(m)
	id, first := pausedHalfway(t, m)
	// Long enough paused that counting it would show.
	time.Sleep(1200 * time.Millisecond)
	finishedIn(t, m, id, first, notices)
}

func TestFinishedInCountsTheRunsBeforeARestart(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(t.TempDir(), "tasks.json")
	m := NewManager(dir, 1)
	m.announce = nil
	m.store = store
	id, first := pausedHalfway(t, m)
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)

	m2 := NewManager(dir, 1)
	m2.store = store
	if err := m2.load(); err != nil {
		t.Fatal(err)
	}
	setLanguage(t, m2, langEN)
	finishedIn(t, m2, id, first, noticesOf(m2))
}

// A godm that dies while a download runs never gets to note how long the run
// took. The list it saved while the download ran has to say.
func TestFinishedInCountsARunThatGodmDidNotSeeEnd(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(t.TempDir(), "tasks.json")
	m := NewManager(dir, 1)
	m.announce = nil
	m.store = store
	src := httptest.NewServer(&slowServer{payload: makePayload(3 << 20), chunk: 16 << 10, delay: 30 * time.Millisecond})
	t.Cleanup(src.Close)
	id, err := m.Add(jobRequest{URL: src.URL + "/film.bin", Connections: 2})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the download to start", stateIs(m, id, StateRunning))
	began := time.Now()
	waitFor(t, 20*time.Second, "half of the file", func() bool {
		v, _ := findTask(m, id)
		return v.Received > 1<<20
	})
	// The last list the dying godm wrote, with the download still running.
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	first := time.Since(began)
	// What follows is only so that the file is free for the godm after it.
	m.Pause(id)
	waitFor(t, 10*time.Second, "the first godm to let go of the file", stateIs(m, id, StatePaused))

	m2 := NewManager(dir, 1)
	m2.store = store
	if err := m2.load(); err != nil {
		t.Fatal(err)
	}
	setLanguage(t, m2, langEN)
	finishedIn(t, m2, id, first, noticesOf(m2))
}

// The same, a step at a time and without a network.
func TestTimeSpentIsCarriedFromRunToRun(t *testing.T) {
	m := NewManager(t.TempDir(), 1)
	m.announce = nil
	const before = 10 * time.Second
	mt := &managedTask{view: TaskView{ID: "x", Filename: "film.bin", Size: 1 << 20, State: StatePaused, Spent: before}}
	m.mu.Lock()
	m.tasks["x"] = mt
	m.order = append(m.order, "x")
	m.mu.Unlock()
	queue := func() int {
		mt.mu.Lock()
		defer mt.mu.Unlock()
		return mt.queueRun()
	}

	// A run that is paused before it ever gets a slot spent no time.
	gen := queue()
	m.Pause("x")
	m.finishRun(mt, gen, StatePaused, "", "")
	if got := mt.snapshot().Spent; got != before {
		t.Errorf("a run that never left the queue changed the time spent from %v to %v", before, got)
	}

	// One that runs adds what it took.
	gen = queue()
	if _, ok := m.acquire(context.Background(), mt, gen); !ok {
		t.Fatal("no slot")
	}
	time.Sleep(60 * time.Millisecond)
	m.finishRun(mt, gen, StatePaused, "", "")
	m.release(laneQueue)
	first := mt.snapshot().Spent
	if first < before+60*time.Millisecond || first > before+time.Second {
		t.Errorf("after a run of 60 ms on top of %v the time spent is %v", before, first)
	}

	// And the one that finishes says the whole of it.
	time.Sleep(800 * time.Millisecond) // paused: not to be counted
	gen = queue()
	if _, ok := m.acquire(context.Background(), mt, gen); !ok {
		t.Fatal("no slot")
	}
	time.Sleep(60 * time.Millisecond)
	m.finishRun(mt, gen, StateDone, "", "")
	m.release(laneQueue)
	v := mt.snapshot()
	took := v.EndedAt.Sub(v.StartedAt)
	if took < first+60*time.Millisecond || took > first+500*time.Millisecond {
		t.Errorf("finished in %v after runs of %v and 60 ms with 800 ms paused between them", took, first)
	}
	if v.Spent != took {
		t.Errorf("the time spent is %v and the time from start to end %v; they are the same thing", v.Spent, took)
	}
}

// A new address with nothing kept from before is a new download, and what the
// old one took is not part of how long it takes.
func TestANewAddressWithNothingKeptStartsTheClockAgain(t *testing.T) {
	m := NewManager(t.TempDir(), 1)
	m.announce = nil
	m.mu.Lock()
	m.running = m.limit // nothing gets a slot, so nothing touches the network
	add := func(id string, v TaskView) *managedTask {
		v.ID, v.State, v.Spent = id, StatePaused, 5*time.Second
		mt := &managedTask{view: v, req: jobRequest{URL: "https://example.invalid/old"}, outDir: t.TempDir()}
		m.tasks[id] = mt
		m.order = append(m.order, id)
		return mt
	}
	empty := add("empty", TaskView{})
	partial := add("partial", TaskView{Path: filepath.Join(t.TempDir(), "film.bin"), Size: 100, Received: 40})
	m.mu.Unlock()

	for _, id := range []string{"empty", "partial"} {
		if err := m.ChangeAddress(id, "https://example.invalid/new"); err != nil {
			t.Fatal(err)
		}
		m.Pause(id)
	}
	if got := empty.snapshot().Spent; got != 0 {
		t.Errorf("a download with nothing saved kept %v of time spent across a new address", got)
	}
	if got := partial.snapshot().Spent; got != 5*time.Second {
		t.Errorf("a download that keeps its progress has %v of time spent, want the 5s it took to get there", got)
	}
}

func TestNotificationSettingSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, 2)
	m.store = dir + "/tasks.json"
	m.SetNotifications(false)
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	m2 := NewManager(dir, 2)
	m2.store = m.store
	if err := m2.load(); err != nil {
		t.Fatal(err)
	}
	if m2.Notifications() {
		t.Error("the notification setting was not restored")
	}
}
