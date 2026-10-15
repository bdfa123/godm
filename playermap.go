package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// A download played while it arrives gets a map of itself in mpv: which parts
// of the file are on disk, which the connections are fetching, and where the
// film is, so that it is plain how far it plays without waiting and where it
// would stop for data. The drawing is done inside mpv, by
// assets/mpv/godm-map.lua, which is embedded here and written beside the maps
// when mpv is opened on a stream. The daemon's part is one small file to a
// download, rewritten about once a second for as long as a player streams the
// download and deleted once none does, and that file is all the script reads.
// It is told the file's name and nothing else: not the daemon's address and
// not the token, so it cannot reach the daemon and holds nothing that would
// let anything else do so.

//go:embed assets/mpv/godm-map.lua
var mapScript []byte

const mapScriptName = "godm-map.lua"

const (
	// defaultMapEvery is how often a map is rewritten while a player streams.
	defaultMapEvery = time.Second
	// defaultMapLinger is how long a map outlives the last stream request of
	// its download. A request ends when the player seeks, or reconnects, and
	// the moment before the next is no reason to take the map down.
	defaultMapLinger = 5 * time.Second
	// mapStale is how old a map has to be to be taken for one a godm left
	// behind when it was stopped: one that is kept up is never more than a
	// second or two old.
	mapStale = time.Minute
)

// playerMap is what a map file says. The byte ranges are [from, to), in
// order and apart from one another.
type playerMap struct {
	Version  int        `json:"v"`
	Size     int64      `json:"size"`
	Have     [][2]int64 `json:"have"`
	Fetching [][2]int64 `json:"fetching"`
	Speed    int64      `json:"speed"`
	State    TaskState  `json:"state"`
	Complete bool       `json:"complete"`
	// Lang is the language the map speaks: godm's Language setting, with
	// auto settled as the tray settles it.
	Lang string `json:"lang"`
	// At is when it was written, in Unix milliseconds, by which the script
	// tells a map that godm has stopped keeping up.
	At int64 `json:"at"`
}

// addRange adds [from, to) after ranges that start before it, joining it to
// the last one when the two meet.
func addRange(rs [][2]int64, from, to int64) [][2]int64 {
	if to <= from {
		return rs
	}
	if n := len(rs); n > 0 && from <= rs[n-1][1] {
		rs[n-1][1] = max(rs[n-1][1], to)
		return rs
	}
	return append(rs, [2]int64{from, to})
}

// mapAhead is how much of a connection's range the map has it fetching: what
// it brings in over this long at its share of the download's speed, and at
// least a byte, which the script draws as a mark where it writes. The rest of
// a range is no nearer for being a connection's. One given the end of a part
// of a large file can have gigabytes still to go, hours of it at a slow link,
// and drawn as on its way all of that would read as nearly there.
const mapAhead = 5 // seconds

// httpMap is the map of an ordinary download, from the segments in its view:
// each holds what it has on disk as one run from its start, and the
// connection of an active one is fetching on from there. The view's segments
// are at most one progress report old, which a map redrawn once a second does
// not notice.
func httpMap(v TaskView) playerMap {
	pm := playerMap{Size: v.Size, Have: [][2]int64{}, Fetching: [][2]int64{}}
	if v.State == StateDone {
		pm.Complete = true
		pm.Have = append(pm.Have, [2]int64{0, v.Size})
		return pm
	}
	active := 0
	for _, s := range v.Segments {
		if s.State == SegActive.String() {
			active++
		}
	}
	ahead := int64(1)
	if active > 0 {
		ahead = max(1, v.Speed/int64(active)*mapAhead)
	}
	for _, s := range v.Segments {
		if s.End < 0 {
			continue // a stream of unknown length, which no player is given
		}
		have := min(s.Start+s.Done, s.End+1)
		pm.Have = addRange(pm.Have, s.Start, have)
		if s.State == SegActive.String() {
			pm.Fetching = addRange(pm.Fetching, have, min(have+ahead, s.End+1))
		}
	}
	return pm
}

// mapRun keeps one download's map for as long as players stream it.
type mapRun struct {
	stop chan struct{}
	once sync.Once
}

// end stops the writing at once, as removing the download does.
func (w *mapRun) end() { w.once.Do(func() { close(w.stop) }) }

// playerMapPath is the file a download's map is kept in, "" when none is
// kept: the manager keeps no maps, or the id is not one godm makes, which a
// list edited by hand could hold and a file name should not.
func (m *Manager) playerMapPath(id string) string {
	const allowed = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_"
	if m.mapDir == "" || id == "" || strings.Trim(id, allowed) != "" {
		return ""
	}
	return filepath.Join(m.mapDir, id+".json")
}

// streamLaunch is how a player is opened on the stream of a download that is
// still arriving: with its map, when one is kept.
func (m *Manager) streamLaunch(mt *managedTask, link, title string) playerLaunch {
	p := playerLaunch{Target: link, Title: title}
	if p.MapFile = m.playerMapPath(mt.snapshot().ID); p.MapFile != "" {
		p.Lang = m.Lang()
	}
	return p
}

// followPlayers is told of each stream request a download has counted among
// its players (see addPlayer). The first sets the writing of its map going,
// and the others find it going.
func (m *Manager) followPlayers(mt *managedTask) {
	path := m.playerMapPath(mt.snapshot().ID)
	if path == "" {
		return
	}
	mt.mu.Lock()
	defer mt.mu.Unlock()
	if mt.removed || mt.mapRun != nil {
		return
	}
	w := &mapRun{stop: make(chan struct{})}
	mt.mapRun = w
	go m.keepPlayerMap(mt, w, path)
}

// keepPlayerMap writes mt's map to path every mapEvery until no player has
// streamed the download for mapLinger, or the download is removed, and then
// deletes it. A download that stops running keeps the map it had, with
// nothing in it being fetched; one that finishes says so, which is the last
// thing a player streaming it is told.
func (m *Manager) keepPlayerMap(mt *managedTask, w *mapRun, path string) {
	dir := filepath.Dir(path)
	os.MkdirAll(dir, 0o700)
	sweepPlayerMaps(dir, mapStale)
	tick := time.NewTicker(m.mapEvery)
	defer tick.Stop()
	var last playerMap
	known := false
	var idle time.Time // since when no stream request has been open
	for {
		mt.mu.Lock()
		if len(mt.players) > 0 {
			idle = time.Time{}
		} else if idle.IsZero() {
			idle = time.Now()
		}
		quit := mt.removed || !idle.IsZero() && time.Since(idle) >= m.mapLinger
		if quit && mt.mapRun == w {
			mt.mapRun = nil
		}
		mt.mu.Unlock()
		if quit {
			dropPlayerMap(mt, path)
			return
		}

		v := mt.snapshot()
		var pm playerMap
		ok := false
		if mt.isTorrent() {
			pm, ok = m.torrentPlayerMap(mt)
		} else if v.Size > 0 && (len(v.Segments) > 0 || v.Received == 0 || v.State == StateDone) {
			// Without segments the view only knows what is on disk when
			// nothing is: a download restored from the list has bytes on
			// disk and no segments until its run first reports, and a map of
			// nothing would have the film wait where it plays at once.
			pm, ok = httpMap(v), true
		}
		switch {
		case ok:
			last, known = pm, true
		case known && v.State == StateDone:
			// A torrent that has finished and left the client.
			last.Have, last.Fetching = [][2]int64{{0, last.Size}}, [][2]int64{}
			last.Complete = true
		case known:
			last.Fetching = [][2]int64{}
		}
		if known {
			last.Version = 1
			last.State, last.Speed = v.State, v.Speed
			last.Complete = last.Complete || v.State == StateDone
			last.Lang = m.Lang()
			last.At = time.Now().UnixMilli()
			putPlayerMap(mt, w, path, last)
		}

		select {
		case <-tick.C:
		case <-w.stop:
		}
	}
}

// putPlayerMap writes pm to path, as long as w still keeps the download's map.
func putPlayerMap(mt *managedTask, w *mapRun, path string, pm playerMap) {
	b, err := json.Marshal(pm)
	if err != nil {
		return
	}
	mt.mapMu.Lock()
	defer mt.mapMu.Unlock()
	mt.mu.Lock()
	mine := mt.mapRun == w && !mt.removed
	mt.mu.Unlock()
	if mine {
		// A write that fails, the script having the file open at that very
		// moment say, is made up for by the next.
		replaceFile(path, b)
	}
}

// dropPlayerMap deletes the map at path once its writer has stopped, unless a
// player has come back and a writer started on it again since: what is there
// then is that one's.
func dropPlayerMap(mt *managedTask, path string) {
	mt.mapMu.Lock()
	defer mt.mapMu.Unlock()
	mt.mu.Lock()
	taken := mt.mapRun != nil && !mt.removed
	mt.mu.Unlock()
	if taken {
		return
	}
	// Windows will not delete a file the script is reading, which it does for
	// a moment once a second.
	for try := 0; try < 10; try++ {
		if err := os.Remove(path); err == nil || errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// replaceFile puts b in place of the file at p in one step, as writeFileAtomic
// does, but does not first flush it to the disk: a map is out of date a
// second after it is written, and all that matters is that the script never
// reads half of one.
func replaceFile(p string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), ".map-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = tmp.Write(b)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(name, p)
	}
	if err != nil {
		os.Remove(name)
	}
	return err
}

// sweepPlayerMaps deletes the maps in dir, and the files they were written
// through, that nothing has written to for as long as older. A godm that is
// stopped while a player streams leaves its map behind, and the next would
// otherwise never delete it.
func sweepPlayerMaps(dir string, older time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !(strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".map-")) {
			continue
		}
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > older {
			os.Remove(filepath.Join(dir, name))
		}
	}
}

// installMapScript writes the overlay script into dir for mpv to load, unless
// it is there already as this godm has it, and says where it is. It is only
// rewritten when it differs, so a player that is running from it is left
// alone when the next is opened, and a godm of another version puts its own
// in its place.
func installMapScript(dir string) (string, error) {
	path := filepath.Join(dir, mapScriptName)
	if b, err := os.ReadFile(path); err == nil && bytes.Equal(b, mapScript) {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := writeFileAtomic(path, mapScript); err != nil {
		return "", err
	}
	return path, nil
}
