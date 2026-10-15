package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
)

// readMap reads the map file at path into pm, reporting whether there was one
// to read.
func readMap(path string, pm *playerMap) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	*pm = playerMap{}
	return json.Unmarshal(b, pm) == nil
}

// checkRanges fails the test unless rs are [from, to) ranges in order, apart
// from one another and inside a file of size bytes.
func checkRanges(t *testing.T, what string, rs [][2]int64, size int64) {
	t.Helper()
	for i, r := range rs {
		if r[0] < 0 || r[0] >= r[1] || r[1] > size || (i > 0 && r[0] <= rs[i-1][1]) {
			t.Fatalf("%s: %v is not a list of separate ranges in order within %d bytes", what, rs, size)
		}
	}
}

// covers reports whether one of rs holds all of [from, to).
func covers(rs [][2]int64, from, to int64) bool {
	for _, r := range rs {
		if r[0] <= from && to <= r[1] {
			return true
		}
	}
	return false
}

func noFile(path string) func() bool {
	return func() bool {
		_, err := os.Stat(path)
		return os.IsNotExist(err)
	}
}

// The manager's menu has to show the number of connections the user chose,
// and the card what the server allows when that is fewer; a page that only
// has the number in use shows the choice undone. Where a player reads is
// told too, but only while one reads, and never kept.
func TestTheTaskViewSaysWhatWasChosenAndWhereAPlayerReads(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, 1)
	m.announce = nil
	m.store = filepath.Join(dir, "tasks.json")
	// Keep the download queued, so nothing here needs the network.
	m.mu.Lock()
	m.running = m.limit
	m.mu.Unlock()
	id, err := m.Add(jobRequest{URL: "https://example.invalid/film.mkv", Connections: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Pause(id)
	mt := m.get(id)
	view := func() TaskView { v, _ := findTask(m, id); return v }
	if v := view(); v.ConnsWanted != 4 || v.Conns != 4 || v.ServerCap != 0 || v.PlayAt != nil {
		t.Fatalf("a new download: conns %d, wanted %d, cap %d, play_at %v", v.Conns, v.ConnsWanted, v.ServerCap, v.PlayAt)
	}

	// The server turns away all but eight connections, as PikPak does. (Once
	// the run is in line: as it begins it forgets any cap.)
	waitFor(t, 5*time.Second, "the run to wait for a slot", func() bool {
		mt.mu.Lock()
		defer mt.mu.Unlock()
		return mt.waitingSlot
	})
	mt.mu.Lock()
	gen := mt.gen
	mt.mu.Unlock()
	mt.progress(gen, Progress{Total: 1 << 30, Limit: 4, ServerCap: 8})
	if _, err := m.SetConnections(id, 12); err != nil {
		t.Fatal(err)
	}
	if v := view(); v.ConnsWanted != 12 || v.Conns != 8 || v.ServerCap != 8 {
		t.Errorf("12 asked for of a server that takes 8: conns %d, wanted %d, cap %d", v.Conns, v.ConnsWanted, v.ServerCap)
	}
	// The engine's next report holds it at the cap, and the choice stays.
	mt.progress(gen, Progress{Total: 1 << 30, Limit: 8, ServerCap: 8})
	if v := view(); v.ConnsWanted != 12 || v.Conns != 8 {
		t.Errorf("after the next report: conns %d, wanted %d", v.Conns, v.ConnsWanted)
	}
	// The answer to the change says what the server takes.
	s := &server{mgr: m, token: "tok"}
	rec := httptest.NewRecorder()
	s.handleConnections(rec, httptest.NewRequest(http.MethodPost, "/api/connections?id="+id+"&n=16", nil))
	var answer struct {
		OK          bool `json:"ok"`
		Connections int  `json:"connections"`
		ServerCap   int  `json:"server_cap"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&answer); err != nil || !answer.OK || answer.Connections != 16 || answer.ServerCap != 8 {
		t.Errorf("the answer to choosing 16: %+v, %v", answer, err)
	}

	// A player reading the download.
	rd := mt.play.NewReader()
	rd.Want(123456)
	if v := view(); v.PlayAt == nil || *v.PlayAt != 123456 {
		t.Errorf("while a player reads at 123456 the view says %v", v.PlayAt)
	}
	b, _ := json.Marshal(view())
	if !bytes.Contains(b, []byte(`"play_at":123456`)) {
		t.Errorf("the API does not say where the player reads: %s", b)
	}
	// The browser's popup gets the few fields it draws, and none of these.
	pb, _ := json.Marshal(popupTasks([]TaskView{view()}))
	for _, f := range []string{"play_at", "conns_wanted", "server_cap"} {
		if bytes.Contains(pb, []byte(f)) {
			t.Errorf("the popup's list has %s in it: %s", f, pb)
		}
	}
	// What is saved holds the choice, and not what lasts only as long as a
	// run or a read.
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	saved, _ := os.ReadFile(m.store)
	if bytes.Contains(saved, []byte("play_at")) || bytes.Contains(saved, []byte("server_cap")) {
		t.Errorf("tasks.json keeps where a player read or the server's cap: %s", saved)
	}
	rd.Close()
	if v := view(); v.PlayAt != nil {
		t.Errorf("with the player gone the view still says %d", *v.PlayAt)
	}
	b, _ = json.Marshal(view())
	if bytes.Contains(b, []byte("play_at")) {
		t.Errorf("with the player gone the API still has play_at: %s", b)
	}

	// A run that ends takes the cap it learnt with it.
	m.Pause(id)
	waitFor(t, 5*time.Second, "the run to end", func() bool { return view().ServerCap == 0 })
	if v := view(); v.ConnsWanted != 16 || v.Conns != 16 {
		t.Errorf("after the run: conns %d, wanted %d", v.Conns, v.ConnsWanted)
	}
}

func TestTheMapOfADownloadIsWhatItsSegmentsHaveOnDisk(t *testing.T) {
	// Two connections at 20 bytes a second between them: 50 bytes each in
	// the few seconds the map looks ahead.
	v := TaskView{Size: 1000, State: StateRunning, Speed: 2 * 50 / mapAhead, Segments: []SegmentView{
		{Start: 0, End: 299, Done: 300, State: "done"},    // finished ranges, merged
		{Start: 300, End: 399, Done: 50, State: "active"}, // runs on from the first
		{Start: 400, End: 599, Done: 0, State: "waiting"}, // nothing yet
		{Start: 600, End: 699, Done: 100, State: "done"},
		{Start: 700, End: 899, Done: 20, State: "active"}, // joins the one before
		{Start: 900, End: 999, Done: 10, State: "retrying"},
	}}
	pm := httpMap(v)
	if want := [][2]int64{{0, 350}, {600, 720}, {900, 910}}; fmt.Sprint(pm.Have) != fmt.Sprint(want) {
		t.Errorf("on disk %v, want %v", pm.Have, want)
	}
	// What the active connections bring in next, and no further than their
	// ranges go: not the rest of a range, which can be hours away. A
	// connection that is retrying fetches nothing until it is through.
	if want := [][2]int64{{350, 400}, {720, 770}}; fmt.Sprint(pm.Fetching) != fmt.Sprint(want) {
		t.Errorf("being fetched %v, want %v", pm.Fetching, want)
	}
	if pm.Complete || pm.Size != 1000 {
		t.Errorf("complete %v, size %d", pm.Complete, pm.Size)
	}
	// Before there is a speed to go by, a mark where each one writes.
	v.Speed = 0
	if want := "[[350 351] [720 721]]"; fmt.Sprint(httpMap(v).Fetching) != want {
		t.Errorf("being fetched at no speed yet %v, want %s", httpMap(v).Fetching, want)
	}

	v.State, v.Segments = StateDone, nil
	pm = httpMap(v)
	if !pm.Complete || fmt.Sprint(pm.Have) != "[[0 1000]]" || len(pm.Fetching) != 0 {
		t.Errorf("a finished download: %+v", pm)
	}
}

// While a player streams a download its map is on disk for mpv, kept up as
// the download goes; it outlives the player only briefly, says when the
// download is complete, and goes with the download.
func TestAMapIsKeptWhilePlayersStreamADownload(t *testing.T) {
	payload := makePayload(8 << 20)
	src := httptest.NewServer(&slowServer{payload: payload, chunk: 8 << 10, delay: 20 * time.Millisecond})
	defer src.Close()

	m := NewManager(t.TempDir(), 2)
	m.announce = nil
	m.streamPiece = 128 << 10
	m.mapDir = filepath.Join(t.TempDir(), "mpv")
	m.mapEvery, m.mapLinger = 50*time.Millisecond, 3*time.Second
	s := &server{mgr: m, token: "tok"}
	mux := http.NewServeMux()
	mux.HandleFunc("/stream/", s.guard(s.handleStream))
	api := httptest.NewServer(mux)
	defer api.Close()

	id, err := m.Add(jobRequest{URL: src.URL + "/film.mkv", Connections: 4})
	if err != nil {
		t.Fatal(err)
	}
	file := m.playerMapPath(id)
	if file != filepath.Join(m.mapDir, id+".json") {
		t.Fatalf("the map of %s is kept at %q", id, file)
	}
	waitFor(t, 10*time.Second, "the download to start", func() bool {
		v, _ := findTask(m, id)
		return v.Received > 0
	})
	if !noFile(file)() {
		t.Fatal("there is a map before any player streams the download")
	}

	link := api.URL + "/stream/" + id + "/film.mkv?token=tok"
	const from, to = 5 << 20, 6<<20 - 1
	type answer struct {
		body []byte
		err  error
	}
	sent := make(chan answer, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodGet, link, nil)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", from, to))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			sent <- answer{err: err}
			return
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		sent <- answer{b, err}
	}()

	var pm playerMap
	waitFor(t, 10*time.Second, "the map", func() bool { return readMap(file, &pm) })
	size := int64(len(payload))
	if pm.Version != 1 || pm.Size != size || pm.State != StateRunning || pm.Complete || pm.Lang != langEN {
		t.Errorf("the map of a download in progress: %+v", pm)
	}
	if age := time.Since(time.UnixMilli(pm.At)); age < 0 || age > 10*time.Second {
		t.Errorf("the map says it was written %s ago", age)
	}
	checkRanges(t, "on disk", pm.Have, size)
	checkRanges(t, "being fetched", pm.Fetching, size)

	var got answer
	select {
	case got = <-sent:
	case <-time.After(30 * time.Second):
		t.Fatal("the player's range never arrived")
	}
	if got.err != nil || sum(got.body) != sum(payload[from:to+1]) {
		t.Fatalf("the range the player was sent: %d bytes, %v", len(got.body), got.err)
	}
	// The player has let go, and the map stays for the moment a seek takes.
	if noFile(file)() {
		t.Fatal("the map went the moment the request ended")
	}
	// Every byte the player was sent was on disk, and the map has it in one
	// run however many connections fetched it.
	waitFor(t, 2500*time.Millisecond, "the map to hold what the player was sent", func() bool {
		return readMap(file, &pm) && covers(pm.Have, from, to+1)
	})
	waitFor(t, 10*time.Second, "the map to go once no player streams the download", noFile(file))

	// A player that stays on while the download finishes is told it is
	// complete. It reads a little and then stops, as a paused one does.
	mt := m.get(id)
	mt.mu.Lock() // the writer reads it under the task's lock
	m.mapLinger = time.Minute
	mt.mu.Unlock()
	resp, err := http.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadFull(resp.Body, make([]byte, 64<<10)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 60*time.Second, "completion", stateIs(m, id, StateDone))
	waitFor(t, 5*time.Second, "a map that says the download is complete", func() bool {
		return readMap(file, &pm) && pm.Complete && pm.State == StateDone &&
			fmt.Sprint(pm.Have) == fmt.Sprintf("[[0 %d]]", size) && len(pm.Fetching) == 0
	})
	// Removing the download takes the map at once, though the player is
	// still there and the map would otherwise stay a minute after it.
	m.Remove(id, false)
	waitFor(t, 3*time.Second, "the map to go with the download", noFile(file))
	entries, _ := os.ReadDir(m.mapDir)
	for _, e := range entries {
		t.Errorf("%s is left in the folder of maps", e.Name())
	}
}

// A download restored from the list has bytes on disk and no segments until
// its run first reports, and a run that starts the file over has the last
// run's segments until then. A map from either would be wrong where it
// matters most, saying the film waits where it plays at once or the other way
// about, so there is no map until there is one to be had.
func TestAMapIsOnlyDrawnFromSegmentsThatAreThisFilesOwn(t *testing.T) {
	m := NewManager(t.TempDir(), 1)
	m.announce = nil
	m.mapDir = filepath.Join(t.TempDir(), "mpv")
	m.mapEvery, m.mapLinger = 20*time.Millisecond, time.Minute
	film := filepath.Join(t.TempDir(), "film.mkv")
	mt := &managedTask{view: TaskView{
		ID: "t1-1", Path: film, Size: 1000, Received: 600, State: StatePaused, Resumable: true,
	}}
	m.mu.Lock()
	m.tasks["t1-1"] = mt
	m.order = append(m.order, "t1-1")
	m.mu.Unlock()
	player, _ := mt.addPlayer(func() {})
	m.followPlayers(mt)
	file := m.playerMapPath("t1-1")
	time.Sleep(200 * time.Millisecond)
	if !noFile(file)() {
		b, _ := os.ReadFile(file)
		t.Errorf("a map before the run said what is on disk: %s", b)
	}
	// The run reports.
	mt.mu.Lock()
	mt.view.State = StateRunning
	mt.view.Segments = []SegmentView{{Start: 0, End: 999, Done: 600, State: "active"}}
	gen := mt.gen
	mt.mu.Unlock()
	var pm playerMap
	waitFor(t, 2*time.Second, "the map of the run", func() bool {
		return readMap(file, &pm) && len(pm.Have) == 1 && pm.Have[0] == [2]int64{0, 600}
	})

	// A run that cannot go on with the file starts another, which has
	// nothing on disk, whatever the last run's segments said.
	mt.onStart(gen, StartInfo{Path: film + " (1)", Size: 1000, Resumable: true})
	waitFor(t, 2*time.Second, "the map of the new file", func() bool {
		return readMap(file, &pm) && len(pm.Have) == 0
	})
	// One that goes on with its file keeps what it has.
	mt.mu.Lock()
	mt.view.Segments, mt.view.Received = []SegmentView{{Start: 0, End: 999, Done: 300, State: "waiting"}}, 300
	mt.mu.Unlock()
	mt.onStart(gen, StartInfo{Path: film + " (1)", Size: 1000, Resumable: true, Resumed: true})
	if v := mt.snapshot(); len(v.Segments) != 1 || v.Received != 300 {
		t.Errorf("a run that resumes lost the segments: %+v", v)
	}

	mt.dropPlayer(player)
	m.Remove("t1-1", false)
	waitFor(t, 3*time.Second, "the map to go with the download", noFile(file))
}

// A torrent's pieces run across its files end to end. The map of one file is
// of the parts of its pieces inside it, counted from its own first byte.
func TestTheMapOfATorrentsFileIsOfItsPartOfThePieces(t *testing.T) {
	run := func(n int, complete, partial bool) torrent.PieceStateRun {
		var r torrent.PieceStateRun
		r.Length, r.Complete, r.Partial = n, complete, partial
		return r
	}
	// Pieces of 100 bytes; the file is bytes 250 to 750 of the torrent, so it
	// begins halfway through piece 2 and ends halfway through piece 7.
	runs := []torrent.PieceStateRun{
		run(3, true, false),  // 0-2: only the end of 2 is the file's
		run(1, false, true),  // 3: arriving
		run(2, true, false),  // 4-5
		run(1, false, false), // 6: nothing yet
		run(1, true, false),  // 7: only its start is the file's
		run(2, false, true),  // 8-9: another file's altogether
	}
	pm := pieceMap(runs, 100, 250, 500)
	if want := "[[0 50] [150 350] [450 500]]"; fmt.Sprint(pm.Have) != want {
		t.Errorf("on disk %v, want %s", pm.Have, want)
	}
	if want := "[[50 150]]"; fmt.Sprint(pm.Fetching) != want {
		t.Errorf("being fetched %v, want %s", pm.Fetching, want)
	}
	if pm.Size != 500 || pm.Complete {
		t.Errorf("size %d, complete %v", pm.Size, pm.Complete)
	}
	if pm := pieceMap([]torrent.PieceStateRun{run(10, true, false)}, 100, 250, 500); !pm.Complete || fmt.Sprint(pm.Have) != "[[0 500]]" {
		t.Errorf("every piece checked: %+v", pm)
	}
}

// A torrent's video gets a map too, from its checked pieces, while a player
// streams it.
func TestATorrentBeingStreamedHasAMapOfItsPieces(t *testing.T) {
	src := t.TempDir()
	film := makePayload(4 << 20)
	// The file before the film is no whole number of pieces long, so the
	// film's first piece is partly that file's.
	mi := makeTorrent(t, src, "Film", 32<<10, tfile{"A.nfo", makePayload(10000)}, tfile{"Film.mkv", film})
	seed := newSeeder(t, src, mi, 512<<10) // about eight seconds for all of it

	m := newBTManager(t)
	m.mapDir = filepath.Join(t.TempDir(), "mpv")
	// Long enough for the map to wait for the second player below; that it
	// goes once the players have is the other test's to show.
	m.mapEvery, m.mapLinger = 50*time.Millisecond, time.Minute
	_, api := btAPI(t, m)
	id, err := m.Add(jobRequest{URL: magnetOf(mi, "Film", seed.addr())})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the file list", func() bool {
		v, _ := findTask(m, id)
		return v.Media != ""
	})
	file := m.playerMapPath(id)
	const from, to = 3 << 20, 3<<20 + 256<<10 - 1
	req := bearer(http.MethodGet, api.URL+"/stream/"+id+"/Film.mkv", nil)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", from, to))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || sum(body) != sum(film[from:to+1]) {
		t.Fatalf("the range: %d bytes, %v", len(body), err)
	}
	size := int64(len(film))
	var pm playerMap
	waitFor(t, 2500*time.Millisecond, "a map that has what the player was sent", func() bool {
		return readMap(file, &pm) && covers(pm.Have, from, to+1)
	})
	if pm.Size != size || pm.State != StateRunning || pm.Complete {
		t.Errorf("the map of a torrent in progress: size %d, %s, complete %v", pm.Size, pm.State, pm.Complete)
	}
	checkRanges(t, "on disk", pm.Have, size)
	checkRanges(t, "being fetched", pm.Fetching, size)

	// A player stays on to the end, reading a little and then nothing more.
	resp, err = http.DefaultClient.Do(bearer(http.MethodGet, api.URL+"/stream/"+id+"/Film.mkv", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadFull(resp.Body, make([]byte, 32<<10)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 40*time.Second, "completion", stateIs(m, id, StateDone))
	// All of the film and nothing of the files beside it.
	waitFor(t, 5*time.Second, "a map of the whole film", func() bool {
		return readMap(file, &pm) && pm.Complete && fmt.Sprint(pm.Have) == fmt.Sprintf("[[0 %d]]", size)
	})
	m.Remove(id, false)
	waitFor(t, 5*time.Second, "the map to go with the torrent", noFile(file))
}

// A map a godm left behind when it stopped is swept away by the next one to
// keep a map there; the maps being kept are left alone.
func TestMapsLeftBehindAreSweptAway(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-2 * mapStale)
	for _, name := range []string{"t1-1.json", ".map-123.tmp", "t2-2.json", "godm-map.lua"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if name != "t2-2.json" {
			os.Chtimes(p, old, old)
		}
	}
	sweepPlayerMaps(dir, mapStale)
	for name, kept := range map[string]bool{"t1-1.json": false, ".map-123.tmp": false, "t2-2.json": true, "godm-map.lua": true} {
		if _, err := os.Stat(filepath.Join(dir, name)); (err == nil) != kept {
			t.Errorf("%s: kept %v, want %v", name, err == nil, kept)
		}
	}
}

// Play opens mpv on a download still arriving with the overlay and the map it
// draws, and a finished file, or VLC, as before.
func TestPlayGivesMPVTheMapOfAStreamAndNothingElse(t *testing.T) {
	var opened []playerLaunch
	old := launchPlayer
	launchPlayer = func(p playerLaunch) error {
		opened = append(opened, p)
		return nil
	}
	defer func() { launchPlayer = old }()

	dir := t.TempDir()
	m := NewManager(dir, 1)
	m.announce = nil
	m.mapDir = filepath.Join(dir, "mpv")
	m.mu.Lock()
	m.running = m.limit // keep the download queued: no network
	m.mu.Unlock()
	s := &server{mgr: m, token: "tok", port: 16801}
	play := func(id string) {
		t.Helper()
		rec := httptest.NewRecorder()
		s.handlePlay(rec, httptest.NewRequest(http.MethodPost, "/api/play?id="+id, nil))
		var out map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&out); err != nil || out["ok"] != true {
			t.Fatalf("play %s: %v %v", id, out, err)
		}
	}
	queued, _ := m.Add(jobRequest{URL: "https://example.invalid/film.mkv", Filename: "film.mkv"})
	defer m.Pause(queued)
	finished := &managedTask{view: TaskView{
		ID: "done", Filename: "clip.mp4", Path: filepath.Join(dir, "clip.mp4"), Size: 10, State: StateDone,
	}}
	m.mu.Lock()
	m.tasks["done"] = finished
	m.order = append(m.order, "done")
	m.mu.Unlock()

	play(queued)
	play("done")
	if len(opened) != 2 {
		t.Fatalf("opened %+v", opened)
	}
	stream, file := opened[0], opened[1]
	if want := filepath.Join(m.mapDir, queued+".json"); stream.MapFile != want || stream.Lang != langEN {
		t.Errorf("the stream was opened with the map %q in %q, want %q in en", stream.MapFile, stream.Lang, want)
	}
	if file.MapFile != "" || file.Target != finished.view.Path {
		t.Errorf("the finished file was opened as %+v", file)
	}

	const script = `C:\godm\mpv\godm-map.lua`
	args := playerArgs(true, stream, script)
	for _, want := range []string{
		"--script=" + script,
		"--script-opts-append=godm-map-file=" + stream.MapFile,
		"--script-opts-append=godm-map-lang=en",
	} {
		if !strings.Contains("\n"+strings.Join(args, "\n")+"\n", "\n"+want+"\n") {
			t.Errorf("mpv on a stream is not given %q: %q", want, args)
		}
	}
	if n := len(args); n < 2 || args[n-2] != "--" || args[n-1] != stream.Target {
		t.Errorf("mpv's command line does not end with the stream: %q", args)
	}
	scripted := func(args []string) bool {
		for _, a := range args {
			if strings.HasPrefix(a, "--script") {
				return true
			}
		}
		return false
	}
	for what, args := range map[string][]string{
		"mpv on a finished file":            playerArgs(true, file, script),
		"VLC on a stream":                   playerArgs(false, stream, script),
		"mpv when the script was not there": playerArgs(true, stream, ""),
	} {
		if scripted(args) {
			t.Errorf("%s is given the overlay: %q", what, args)
		}
	}
	if args := playerArgs(false, stream, script); strings.Join(args, " ") != "--network-caching=10000 --meta-title=film.mkv "+stream.Target {
		t.Errorf("VLC's command line changed: %q", args)
	}

	// A manager that keeps no maps offers none, and an id that is no file
	// name gets none.
	m.mapDir = ""
	play(queued)
	if opened[2].MapFile != "" {
		t.Errorf("a manager without maps opened mpv with %q", opened[2].MapFile)
	}
	m.mapDir = dir
	for _, id := range []string{"", "../t1", `a\b`, "t1.json"} {
		if p := m.playerMapPath(id); p != "" {
			t.Errorf("the id %q has its map at %q", id, p)
		}
	}
}

func TestTheMapScriptIsWrittenForMPVWhenItDiffers(t *testing.T) {
	for _, part := range []string{`options.read_options(opts, "godm-map")`, `mp.add_key_binding("h", "toggle"`, `"user-data/godm-map"`} {
		if !bytes.Contains(mapScript, []byte(part)) {
			t.Fatalf("the embedded script has no %s; is it the overlay?", part)
		}
	}
	dir := filepath.Join(t.TempDir(), "mpv")
	path, err := installMapScript(dir)
	if err != nil || path != filepath.Join(dir, mapScriptName) {
		t.Fatalf("installMapScript = %q, %v", path, err)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, mapScript) {
		t.Fatal("what was written is not the script")
	}
	// The same script is left as it is, for a player that may be loading it.
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	os.Chtimes(path, old, old)
	if _, err := installMapScript(dir); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); !fi.ModTime().Equal(old) {
		t.Error("the same script was written again")
	}
	// Another godm's is replaced.
	os.WriteFile(path, []byte("-- an older godm's overlay"), 0o644)
	if _, err := installMapScript(dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, mapScript) {
		t.Error("an older script was left in place")
	}
}

// The key that switches the map on is said in the README and in the script's
// own notes, and has to be the one the script binds.
func TestTheMapKeyIsTheOneTheNotesName(t *testing.T) {
	m := regexp.MustCompile(`mp\.add_key_binding\("([^"]+)", "toggle"`).FindSubmatch(mapScript)
	if m == nil {
		t.Fatal("the script binds no key to toggle")
	}
	key := string(m[1])
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"**" + key + "**", "script-binding godm_map/toggle"} {
		if !bytes.Contains(readme, []byte(want)) {
			t.Errorf("the README does not say %s", want)
		}
	}
	if !bytes.Contains(mapScript, []byte("switched on with "+key+".")) {
		t.Errorf("the script's notes do not name %s as the key", key)
	}
}

// ---------- the script in mpv itself ----------

// silentWAV is a WAV file of secs seconds of silence, 8000 bytes to a second,
// with pad bytes the player skips before the samples (an even number, or
// none): a file mpv plays with no window and no sound card, whose duration it
// knows. With a pad, its bytes are out of proportion with its time, as a
// film's are where its bitrate rises or its container keeps an index.
func silentWAV(secs, pad int) []byte {
	const rate = 8000
	n := rate * secs
	le := binary.LittleEndian
	b := []byte("RIFF")
	b = le.AppendUint32(b, 0) // filled in below
	b = append(b, "WAVEfmt "...)
	b = le.AppendUint32(b, 16)
	b = le.AppendUint16(b, 1) // PCM
	b = le.AppendUint16(b, 1) // mono
	b = le.AppendUint32(b, rate)
	b = le.AppendUint32(b, rate)
	b = le.AppendUint16(b, 1)
	b = le.AppendUint16(b, 8)
	if pad > 0 {
		b = append(b, "JUNK"...)
		b = le.AppendUint32(b, uint32(pad))
		b = append(b, make([]byte, pad)...)
	}
	b = append(b, "data"...)
	b = le.AppendUint32(b, uint32(n))
	b = append(b, bytes.Repeat([]byte{128}, n)...)
	le.PutUint32(b[4:], uint32(len(b)-8))
	return b
}

// mpvIPC talks to mpv over its JSON IPC: a named pipe on Windows, a socket
// elsewhere.
type mpvIPC struct {
	conn io.ReadWriteCloser
	r    *bufio.Reader
	proc *os.Process
	seq  int
}

func dialMPV(t *testing.T, addr string, proc *os.Process) *mpvIPC {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var c io.ReadWriteCloser
		var err error
		if runtime.GOOS == "windows" {
			c, err = os.OpenFile(addr, os.O_RDWR, 0)
		} else {
			c, err = net.Dial("unix", addr)
		}
		if err == nil {
			return &mpvIPC{conn: c, r: bufio.NewReader(c), proc: proc}
		}
		if time.Now().After(deadline) {
			t.Fatalf("mpv's IPC never opened: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// do runs a command and returns its data, or nil when mpv said it failed.
func (c *mpvIPC) do(t *testing.T, args ...any) json.RawMessage {
	t.Helper()
	c.seq++
	b, _ := json.Marshal(map[string]any{"command": args, "request_id": c.seq})
	// A reply that never comes would leave the read below waiting for ever;
	// ending mpv ends the read.
	watchdog := time.AfterFunc(10*time.Second, func() { c.proc.Kill() })
	defer watchdog.Stop()
	if _, err := c.conn.Write(append(b, '\n')); err != nil {
		t.Fatalf("mpv %v: %v", args, err)
	}
	for {
		line, err := c.r.ReadBytes('\n')
		if err != nil {
			t.Fatalf("mpv %v: no reply: %v", args, err)
		}
		var reply struct {
			ID    int             `json:"request_id"`
			Error string          `json:"error"`
			Data  json.RawMessage `json:"data"`
		}
		if json.Unmarshal(line, &reply) != nil || reply.ID != c.seq {
			continue // an event, or the reply to something else
		}
		if reply.Error != "success" {
			return nil
		}
		return reply.Data
	}
}

// overlayState is what the script publishes in user-data/godm-map.
type overlayState struct {
	Known      bool    `json:"known"`
	Visible    bool    `json:"visible"`
	Toggled    bool    `json:"toggled"`
	Complete   bool    `json:"complete"`
	Slow       bool    `json:"slow"`
	Stale      bool    `json:"stale"`
	Stalled    bool    `json:"stalled"`
	Ranges     float64 `json:"ranges"`
	Fetching   float64 `json:"fetching"`
	Shapes     float64 `json:"shapes"`
	Bottom     float64 `json:"bottom"`
	Playhead   float64 `json:"playhead"`
	MapReady   float64 `json:"map_ready"`
	ReadyUntil float64 `json:"ready_until"`
	Need       float64 `json:"need"`
	Lang       string  `json:"lang"`
	Text       string  `json:"text"`
}

func (c *mpvIPC) overlay(t *testing.T) overlayState {
	t.Helper()
	var s overlayState
	if data := c.do(t, "get_property", "user-data/godm-map"); data != nil {
		json.Unmarshal(data, &s)
	}
	return s
}

// wait asks the overlay what it says until cond holds of it.
func (c *mpvIPC) wait(t *testing.T, what string, cond func(overlayState) bool) overlayState {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		s := c.overlay(t)
		if cond(s) {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %s: the overlay says %+v", what, s)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// startMPV opens mpv as Play opens it on p, with the overlay script, but with
// no window, no sound and none of the user's own settings, with extra on its
// command line, and with an IPC to ask it what the script found. It is ended
// with the test, and the test fails on anything the script complained of.
func startMPV(t *testing.T, exe string, p playerLaunch, script string, extra ...string) *mpvIPC {
	t.Helper()
	ipc := fmt.Sprintf(`\\.\pipe\godm-map-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	if runtime.GOOS != "windows" {
		ipc = filepath.Join(t.TempDir(), "mpv.sock")
	}
	logFile := filepath.Join(t.TempDir(), "mpv.log")
	args := append([]string{"--no-config", "--vo=null", "--ao=null", "--force-window=no", "--idle=no",
		"--input-ipc-server=" + ipc, "--log-file=" + logFile}, extra...)
	for _, a := range playerArgs(true, p, script) {
		if !strings.HasPrefix(a, "--force-window") && !strings.HasPrefix(a, "--cache=") {
			args = append(args, a)
		}
	}
	cmd := exec.Command(exe, args...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := dialMPV(t, ipc, cmd.Process)
	t.Cleanup(func() {
		c.conn.Close()
		cmd.Process.Kill()
		cmd.Wait()
		log, _ := os.ReadFile(logFile)
		for _, line := range strings.Split(string(log), "\n") {
			if strings.Contains(line, "[godm_map]") && (strings.Contains(line, "[e]") || strings.Contains(line, "[w]")) {
				t.Errorf("mpv: %s", strings.TrimSpace(line))
			}
		}
	})
	return c
}

// The overlay itself, run in the mpv on this machine with no window and no
// sound: it reads the map godm writes, works out how far playback runs from
// the playhead, survives a map it cannot read, follows the map to the end of
// the download and away, makes way for subtitles, and shows itself when it
// should. Where there is no mpv there is nothing to run it in.
func TestTheMapScriptInMPV(t *testing.T) {
	exe, isMPV := findPlayer()
	if !isMPV {
		t.Skip("mpv is not installed")
	}
	dir := filepath.Join(t.TempDir(), "a, folder=with commas")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const secs = 60
	wav := silentWAV(secs, 0)
	film := filepath.Join(dir, "film.wav")
	if err := os.WriteFile(film, wav, 0o600); err != nil {
		t.Fatal(err)
	}
	script, err := installMapScript(dir)
	if err != nil {
		t.Fatal(err)
	}
	size := int64(len(wav))
	mapFile := filepath.Join(dir, "t1-1.json")
	put := func(pm playerMap) {
		t.Helper()
		pm.Version, pm.Size, pm.At = 1, size, time.Now().UnixMilli()
		b, _ := json.Marshal(pm)
		if err := replaceFile(mapFile, b); err != nil {
			t.Fatal(err)
		}
	}
	// Half of it is on disk, and the download is too slow for it: a film of
	// this length needs 8000 bytes a second.
	put(playerMap{
		Have: [][2]int64{{0, size / 2}}, Fetching: [][2]int64{{size / 2, size/2 + 4000}},
		Speed: 4000, State: StateRunning, Lang: langZH,
	})

	// It starts paused, and reads ahead as little as it can, so that what it
	// has buffered does not stand in for what the map says.
	c := startMPV(t, exe, playerLaunch{Target: film, Title: "film", MapFile: mapFile, Lang: langEN}, script,
		"--pause", "--cache=no", "--demuxer-readahead-secs=0")

	s := c.wait(t, "the map", func(s overlayState) bool { return s.Known })
	near := func(a, b float64) bool { return a > b-1 && a < b+1 }
	// The playhead is the byte mpv plays, a little past the start by what its
	// decoder holds.
	if !s.Visible || s.Ranges != 1 || s.Fetching != 1 || s.Playhead < 0 || s.Playhead > 8000 || !near(s.MapReady, secs/2) ||
		s.ReadyUntil < s.MapReady || !near(s.Need, float64(size)/secs) || !s.Slow || s.Complete || s.Stale || s.Stalled {
		t.Errorf("paused at the start of a half-downloaded film: %+v", s)
	}
	// The map said Chinese, and the map has the last word over mpv's options.
	if s.Lang != langZH || !strings.Contains(s.Text, "可连续播放到 0:30") || !strings.Contains(s.Text, "太慢") {
		t.Errorf("the words over the map: %s %q", s.Lang, s.Text)
	}
	if s.Shapes < 5 {
		t.Errorf("drew %v shapes, want the bar, what is on disk, what is coming, the playhead and the words", s.Shapes)
	}

	// Half a file, as a write cut short would leave it: the last map stands.
	if err := os.WriteFile(mapFile, []byte(`{"v":1,"size":`), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	if s := c.overlay(t); !s.Known || s.Ranges != 1 {
		t.Errorf("a map that could not be read lost the last one: %+v", s)
	}

	// Further on, past what is on disk.
	put(playerMap{Have: [][2]int64{{0, size / 4}}, Speed: 9000, State: StateRunning, Lang: langEN})
	c.do(t, "seek", 40, "absolute")
	s = c.wait(t, "the playhead past what is on disk", func(s overlayState) bool { return s.Lang == langEN && s.Playhead > float64(size)/2 })
	if s.MapReady != -1 || s.Slow || !strings.Contains(s.Text, "still downloading") {
		t.Errorf("at 0:40 of a film on disk to 0:15: %+v", s)
	}

	// The download finishes. (Back near the start, so that the film does not
	// run out while the rest of this plays it.)
	put(playerMap{Have: [][2]int64{{0, size}}, Complete: true, State: StateDone, Lang: langEN})
	c.do(t, "seek", 5, "absolute")
	s = c.wait(t, "the map to say it is complete", func(s overlayState) bool { return s.Complete })
	if !strings.Contains(s.Text, "Fully downloaded") || !near(s.ReadyUntil, secs) {
		t.Errorf("a complete download: %+v", s)
	}

	// Subtitles are read with the film paused, which is when the map shows.
	// While there are any, it goes above where two lines of mpv's reach, 125
	// of the 720 lines up with its own settings; with none, it sits low, just
	// above the controller. (With no window the script lays out 720 lines.)
	low := c.wait(t, "the map to be drawn", func(s overlayState) bool { return s.Bottom > 0 }).Bottom
	if low <= 720-125 {
		t.Errorf("with no subtitles the map is drawn up at %v", low)
	}
	srt := filepath.Join(dir, "film.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:00,000 --> 00:01:00,000\nA line\nand another\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.do(t, "sub-add", srt)
	c.wait(t, "the map to make way for subtitles", func(s overlayState) bool { return s.Bottom > 0 && s.Bottom <= 720-125 })
	c.do(t, "set_property", "sid", "no")
	c.wait(t, "the map to come back down", func(s overlayState) bool { return s.Bottom == low })

	// Switched on, it shows while the film plays.
	c.do(t, "script-binding", "godm_map/toggle")
	c.do(t, "set_property", "pause", false)
	if s := c.wait(t, "the switch", func(s overlayState) bool { return s.Toggled }); !s.Visible {
		t.Errorf("switched on, playing: %+v", s)
	}
	// Switched off, it goes once a seek's few seconds are up.
	c.do(t, "script-binding", "godm_map/toggle")
	c.wait(t, "the map to hide while the film plays", func(s overlayState) bool { return !s.Toggled && !s.Visible })
	c.do(t, "seek", 10, "absolute")
	c.wait(t, "the map to show after a seek", func(s overlayState) bool { return s.Visible })
	c.wait(t, "the map to hide again", func(s overlayState) bool { return !s.Visible })

	// godm deletes the map once nothing streams the download: so goes the map.
	os.Remove(mapFile)
	c.wait(t, "the map to go", func(s overlayState) bool { return !s.Known && !s.Visible })
	c.do(t, "quit")
}

// A film played as it arrives catches up with a download slower than itself,
// and stops to wait at the end of what is on disk. The overlay says it is
// waiting then, with the playhead on that edge, however far the film's bytes
// are out of proportion with its time: here more than a quarter of the file
// comes before the first sample. Put in proportion to the time, the playhead
// would sit far back in what is on disk, and the overlay would say that the
// film plays on.
func TestTheMapScriptSaysAFilmWaitingForDataIsWaiting(t *testing.T) {
	exe, isMPV := findPlayer()
	if !isMPV {
		t.Skip("mpv is not installed")
	}
	const secs, pad, rate = 60, 400000, 8000
	wav := silentWAV(secs, pad)
	size := int64(len(wav))
	// On disk: the pad and the first nine seconds of the film.
	front := size - (secs-9)*rate
	// The stream of a download serves what is on disk and waits for the rest.
	release := make(chan struct{})
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		from := int64(0)
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &from)
		if from < 0 || from >= size {
			http.Error(w, "out of range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", fmt.Sprint(size-from))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, size-1, size))
		w.WriteHeader(http.StatusPartialContent)
		if from < front {
			w.Write(wav[from:front])
			w.(http.Flusher).Flush()
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer src.Close()
	defer close(release)

	dir := t.TempDir()
	script, err := installMapScript(dir)
	if err != nil {
		t.Fatal(err)
	}
	mapFile := filepath.Join(dir, "t1-1.json")
	put := func(size int64) {
		t.Helper()
		b, _ := json.Marshal(playerMap{
			Version: 1, Size: size, Have: [][2]int64{{0, front}}, Fetching: [][2]int64{{front, front + 1}},
			Speed: 1000, State: StateRunning, Lang: langEN, At: time.Now().UnixMilli(),
		})
		if err := replaceFile(mapFile, b); err != nil {
			t.Fatal(err)
		}
	}
	put(size)
	// Played with the cache Play gives it, and fast, so that it is at the end
	// of what is on disk in a couple of seconds.
	c := startMPV(t, exe, playerLaunch{Target: src.URL + "/film.wav", Title: "film", MapFile: mapFile, Lang: langEN}, script,
		"--cache=yes", "--speed=4")
	s := c.wait(t, "playback to stop for data", func(s overlayState) bool { return s.Known && s.Stalled })
	if !strings.Contains(s.Text, "still downloading") {
		t.Errorf("waiting for data, the overlay says %q", s.Text)
	}
	if s.Playhead > float64(front) || s.Playhead < float64(front-rate) {
		t.Errorf("waiting for byte %d, the playhead is at %v", front, s.Playhead)
	}
	if !s.Visible {
		t.Errorf("waiting for data, the map is not shown: %+v", s)
	}

	// Where mpv's place in the stream is not to be had, a map of another size
	// standing in here, the playhead is put in proportion to the time: at 0:09
	// of the minute, far back in what is on disk, with what reads as most of
	// half a minute of film after it. While playback waits for data, that is
	// no reason to say it plays on.
	put(size - 1)
	s = c.wait(t, "the playhead to be put in proportion", func(s overlayState) bool {
		return s.Playhead >= 0 && s.Playhead < float64(front)/2
	})
	if s.MapReady < 20 {
		t.Fatalf("in proportion the map should run on well past 0:09: %+v", s)
	}
	if !s.Stalled || !strings.Contains(s.Text, "still downloading") {
		t.Errorf("waiting for data, with the playhead in proportion: %+v", s)
	}
	c.do(t, "quit")
}

// A seek into a part of the film that has not arrived leaves mpv seeking and
// not paused for the cache: the picture stands still and nothing plays until
// the first bytes there come, which on a slow download is long after the few
// seconds a seek brings the map up for. The map is wanted most then, and it
// stays up for as long as the wait does.
func TestTheMapScriptStaysUpWhileASeekWaitsForData(t *testing.T) {
	exe, isMPV := findPlayer()
	if !isMPV {
		t.Skip("mpv is not installed")
	}
	const secs, rate = 60, 8000
	wav := silentWAV(secs, 0)
	size := int64(len(wav))
	// On disk: the first ten seconds.
	front := int64(10 * rate)
	// The stream of a download serves what is on disk and, past it, sends its
	// headers and then waits for bytes that do not come.
	release := make(chan struct{})
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		from := int64(0)
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &from)
		if from < 0 || from >= size {
			http.Error(w, "out of range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", fmt.Sprint(size-from))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, size-1, size))
		w.WriteHeader(http.StatusPartialContent)
		if from < front {
			w.Write(wav[from:front])
		}
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer src.Close()
	defer close(release)

	dir := t.TempDir()
	script, err := installMapScript(dir)
	if err != nil {
		t.Fatal(err)
	}
	mapFile := filepath.Join(dir, "t1-1.json")
	b, _ := json.Marshal(playerMap{
		Version: 1, Size: size, Have: [][2]int64{{0, front}}, Fetching: [][2]int64{{front, front + 1}},
		Speed: 1000, State: StateRunning, Lang: langEN, At: time.Now().UnixMilli(),
	})
	if err := replaceFile(mapFile, b); err != nil {
		t.Fatal(err)
	}
	c := startMPV(t, exe, playerLaunch{Target: src.URL + "/film.wav", Title: "film", MapFile: mapFile, Lang: langEN}, script,
		"--cache=yes")
	c.wait(t, "the map", func(s overlayState) bool { return s.Known })
	flag := func(prop string) bool {
		var on bool
		json.Unmarshal(c.do(t, "get_property", prop), &on)
		return on
	}

	c.do(t, "seek", 40, "absolute")
	c.wait(t, "the map to show after a seek", func(s overlayState) bool { return s.Visible })
	// Well past the seek's few seconds (linger in the script), the film is
	// still waiting for the bytes at 0:40, and so mpv says in its own words.
	time.Sleep(4500 * time.Millisecond)
	if !flag("seeking") || flag("paused-for-cache") {
		t.Fatalf("the seek was to wait for data without pausing for the cache: seeking %v, paused for the cache %v",
			flag("seeking"), flag("paused-for-cache"))
	}
	if s := c.overlay(t); !s.Visible {
		t.Errorf("the film waits for data after a seek, and the map is not shown: %+v", s)
	}
	c.do(t, "quit")
}
