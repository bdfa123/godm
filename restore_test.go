package main

import (
	"path/filepath"
	"testing"
	"time"
)

// For a torrent the task's connection count is the number of seeds it is
// connected to, and its card says "N peers · M seeds" from it. A torrent that
// is not running is connected to nobody, so the number has to be nothing:
// neither the connection count a file download would be given, nor whoever
// was there when it last ran.

func TestARestoredTorrentHasNoSeeds(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "tasks.json")
	const magnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=film"

	m := NewManager(dir, 2)
	m.store = store
	add := func(id string, v TaskView, req jobRequest) {
		v.ID, v.AddedAt = id, time.Now()
		m.tasks[id] = &managedTask{view: v, req: req, outDir: dir}
		m.order = append(m.order, id)
	}
	// As Add leaves a torrent: no connection count asked for, none shown.
	add("paused", TaskView{Kind: "bt", State: StatePaused}, jobRequest{URL: magnet, Kind: "bt", Connections: 8})
	// As a godm that died leaves one that was running, and one that seeded.
	add("running", TaskView{Kind: "bt", State: StateRunning, Conns: 3}, jobRequest{URL: magnet + "1", Kind: "bt", Connections: 8})
	add("seeding", TaskView{Kind: "bt", State: StateDone, Stage: "seeding", Conns: 2}, jobRequest{URL: magnet + "2", Kind: "bt", Connections: 8})
	// A file keeps the count it was given.
	add("file", TaskView{State: StatePaused, Conns: 4}, jobRequest{URL: "https://h.example/a.bin", Connections: 4})
	if err := m.save(); err != nil {
		t.Fatal(err)
	}

	m2 := NewManager(dir, 2)
	m2.store = store
	if err := m2.load(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"paused", "running", "seeding"} {
		if v, ok := findTask(m2, id); !ok || v.Conns != 0 {
			t.Errorf("the torrent %q came back with %d seeds (listed: %v); it is connected to nobody", id, v.Conns, ok)
		}
	}
	if v, _ := findTask(m2, "file"); v.Conns != 4 {
		t.Errorf("the file came back with %d connections, want the 4 it was given", v.Conns)
	}
}

// The same number when a torrent stops without godm restarting: the run that
// ends takes its peers with it.
func TestAStoppedTorrentHasNoSeeds(t *testing.T) {
	for _, st := range []TaskState{StatePaused, StateError} {
		m := NewManager(t.TempDir(), 1)
		m.announce = nil
		mt := &managedTask{
			view: TaskView{ID: "bt", Kind: "bt", State: StateRunning, Active: 5, Conns: 3},
			req:  jobRequest{URL: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567", Kind: "bt"},
		}
		mt.gen = 1
		m.finishRun(mt, 1, st, "", "")
		if v := mt.snapshot(); v.Active != 0 || v.Conns != 0 {
			t.Errorf("a torrent that ended %s still shows %d peers · %d seeds", st, v.Active, v.Conns)
		}
	}

	// A file's count is how many connections it may use, running or not.
	m := NewManager(t.TempDir(), 1)
	m.announce = nil
	mt := &managedTask{view: TaskView{ID: "f", State: StateRunning, Active: 4, Conns: 8}}
	mt.gen = 1
	m.finishRun(mt, 1, StatePaused, "", "")
	if v := mt.snapshot(); v.Conns != 8 {
		t.Errorf("a paused file shows %d connections, want the 8 it may use", v.Conns)
	}

	// And a torrent that finished goes on to seed with the peers it has; what
	// it shows then is the seeding's to say.
	mt = &managedTask{view: TaskView{ID: "s", Kind: "bt", State: StateRunning, Stage: "seeding", Active: 5, Conns: 3}}
	mt.gen = 1
	m.finishRun(mt, 1, StateDone, "", "")
	if v := mt.snapshot(); v.Conns != 3 {
		t.Errorf("a torrent that finished and seeds shows %d seeds, want the 3 it is connected to", v.Conns)
	}
}
