//go:build windows

package main

import (
	"os"
	"strings"
	"syscall"
	"testing"
)

// holdOpen opens a file the way a virus scanner or a backup program can: so
// that nobody else may read it, write it or move it until it is let go.
func holdOpen(t *testing.T, path string) (release func()) {
	t.Helper()
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release = func() {
		if !released {
			released = true
			syscall.CloseHandle(h)
		}
	}
	t.Cleanup(release)
	return release
}

// A list that another program has open can be neither read nor moved aside,
// and there is nothing wrong with it. godm cannot show it this time, but it
// must not save an empty list over it once the other program lets go: it
// keeps its hands off the file and the next start reads it.
func TestAListThatCanBeNeitherReadNorMovedIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	store := writeStore(t, dir, "tasks.json", []byte(goodList))
	release := holdOpen(t, store)

	m, notices := stored(t, dir)
	err := m.load()
	if err == nil || !strings.Contains(err.Error(), "nothing is saved until godm is restarted") {
		t.Errorf("load: %v", err)
	}
	if len(m.List()) != 0 {
		t.Errorf("tasks came out of a file that could not be opened: %+v", m.List())
	}
	m.announceLoad()
	want := "The task list could not be read | It could not be set aside either, so nothing is saved until godm is restarted. | app"
	if len(*notices) != 1 || (*notices)[0] != want {
		t.Errorf("the user was told %q\nwant %q", *notices, want)
	}

	// The other program lets go, and godm goes on being used.
	release()
	m.SetNotifications(true)
	m.SetLimit(9)
	for i := 0; i < 3; i++ {
		if err := m.save(); err != nil {
			t.Fatal(err)
		}
	}
	if b, _ := os.ReadFile(store); string(b) != goodList {
		t.Fatalf("the list was written over; it is now %s", b)
	}
	if got := filesIn(t, dir); len(got) != 1 {
		t.Errorf("the folder holds %v, want only the list as it was", got)
	}

	m2, _ := stored(t, dir)
	if err := m2.load(); err != nil {
		t.Fatal(err)
	}
	hasGoodList(t, m2)
}

// With a backup to restore, the held file is no reason to stop saving: what
// was restored is one save short of it.
func TestAHeldListWithABackupIsRestoredFromTheBackup(t *testing.T) {
	dir := t.TempDir()
	store := writeStore(t, dir, "tasks.json", []byte(strings.Replace(goodList, `"limit":5`, `"limit":6`, 1)))
	writeStore(t, dir, "tasks.json.bak", []byte(goodList))
	holdOpen(t, store)

	m, notices := stored(t, dir)
	if err := m.load(); err == nil || !strings.Contains(err.Error(), "the last backup was restored") {
		t.Errorf("load: %v", err)
	}
	hasGoodList(t, m)
	if m.store == "" {
		t.Errorf("saving was switched off although the backup was restored")
	}
	// Nothing was set aside, so the notice must not say that anything was.
	m.announceLoad()
	if want := "The task list could not be read | The last backup was restored. | app"; len(*notices) != 1 || (*notices)[0] != want {
		t.Errorf("the user was told %q\nwant %q", *notices, want)
	}
}
