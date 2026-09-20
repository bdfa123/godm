package main

import (
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
