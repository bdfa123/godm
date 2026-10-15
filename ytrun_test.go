package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- the temporary folder ----------

// The released yt-dlp.exe unpacks itself into a folder of the temporary
// folder, and its launcher takes it away when the program ends. Ended by
// godm, it never gets to: every pause and every removal left 23 MB behind.

func TestYTDLPGetsATemporaryFolderOfItsOwnAndItGoesWhenDone(t *testing.T) {
	cmd := exec.Command("yt-dlp")
	cmd.Env = []string{"A=1", "temp=somewhere shared"}
	done := prepareEnv(cmd)
	var dirs []string
	for _, name := range []string{"TMP", "TEMP", "TMPDIR"} {
		v, ok := envValue(cmd.Env, name)
		if !ok {
			t.Fatalf("%s is not set in %q", name, cmd.Env)
		}
		dirs = append(dirs, v)
	}
	if dirs[0] != dirs[1] || dirs[1] != dirs[2] {
		t.Fatalf("the temporary folders are not one: %q", dirs)
	}
	dir := dirs[0]
	if dir == "somewhere shared" || !strings.HasPrefix(filepath.Base(dir), "godm-yt-") {
		t.Fatalf("the temporary folder is %q, want one made for this run", dir)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("the folder was not made: %v", err)
	}
	for _, kv := range cmd.Env {
		if strings.Contains(kv, "somewhere shared") {
			t.Errorf("the environment still has %q beside the one that was set: %q", kv, cmd.Env)
		}
	}
	os.WriteFile(filepath.Join(dir, "something"), []byte("yt-dlp's"), 0o600)
	done()
	if exists(dir) {
		t.Error("the folder is still there when the run is over")
	}
}

func TestWhatYTDLPUnpacksIsRemovedWhenItEnds(t *testing.T) {
	stubYTDLP(t, "temp")
	got, err := RunYTDLP(context.Background(), YTDLPOptions{
		URL: "https://example.test/watch?v=1", OutDir: t.TempDir(), Binary: "stub",
	})
	if err != nil {
		t.Fatal(err)
	}
	dirs := strings.Split(got, "|")
	if len(dirs) != 3 || dirs[0] != dirs[1] || dirs[1] != dirs[2] {
		t.Fatalf("the child was given the temporary folders %q, want one folder for all three", dirs)
	}
	if dirs[0] == os.TempDir() || filepath.Clean(dirs[0]) == filepath.Clean(os.TempDir()) {
		t.Fatalf("the child unpacked into the shared temporary folder, %s", dirs[0])
	}
	waitFor(t, 5*time.Second, "the child's temporary folder to go", func() bool { return !exists(dirs[0]) })
}

func TestWhatYTDLPUnpacksIsRemovedWhenItIsStopped(t *testing.T) {
	stubYTDLP(t, "litter")
	beatFile(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var temp string
	done := make(chan error, 1)
	go func() {
		_, err := RunYTDLP(ctx, YTDLPOptions{
			URL: "https://example.test/watch?v=1", OutDir: t.TempDir(), Binary: "stub",
			// The stand-in has unpacked by the time it names its temporary folder.
			OnStart: func(s YTStart) {
				mu.Lock()
				temp = s.Title
				mu.Unlock()
				cancel()
			},
		})
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("RunYTDLP never came back")
	}
	mu.Lock()
	defer mu.Unlock()
	if temp == "" {
		t.Fatal("the child never said where it unpacked")
	}
	if filepath.Clean(temp) == filepath.Clean(os.TempDir()) {
		t.Fatalf("the child unpacked into the shared temporary folder, %s", temp)
	}
	waitFor(t, 10*time.Second, "what the stopped child unpacked to go", func() bool { return !exists(temp) })
}

// Windows will not delete a file that a program has open, and the programs of a
// download may only let go a moment after they are stopped.
func TestAFolderThatCannotBeRemovedYetIsRemovedWhenItCan(t *testing.T) {
	was := deleteFor
	deleteFor = 10 * time.Second
	t.Cleanup(func() { deleteFor = was })

	dir := filepath.Join(t.TempDir(), "godm-yt-busy")
	if err := os.MkdirAll(filepath.Join(dir, "in"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "in", "use.dll"), []byte("held open"), 0o600); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(filepath.Join(dir, "in", "use.dll"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	removeTree(dir, "a folder of a test")
	time.Sleep(200 * time.Millisecond)
	held.Close()
	waitFor(t, 5*time.Second, "the folder to be removed once nothing held it", func() bool { return !exists(dir) })
}

// ---------- when yt-dlp cannot find Deno ----------

// Without a JavaScript runtime YouTube hands out no formats, and yt-dlp says
// so in a warning and then fails with an error that does not: the video "is
// not available". Which sent whoever read it to look at the video.
func TestAMissingJavaScriptRuntimeIsSaidSoInTheReason(t *testing.T) {
	stubYTDLP(t, "nojs")
	_, err := RunYTDLP(context.Background(), YTDLPOptions{
		URL: "https://example.test/watch?v=1", OutDir: t.TempDir(), Binary: "stub",
	})
	if err == nil {
		t.Fatal("expected a failure")
	}
	for _, want := range []string{"This video is not available", "JavaScript runtime", "DenoLand.Deno"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("download: %q leaves out %q", err, want)
		}
	}

	_, err = InspectYTDLP(context.Background(), YTDLPOptions{
		URL: "https://example.test/watch?v=1", Binary: "stub",
	})
	if err == nil {
		t.Fatal("expected a failure")
	}
	for _, want := range []string{"This video is not available", "JavaScript runtime", "DenoLand.Deno"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("inspection: %q leaves out %q", err, want)
		}
	}
}

func TestAReasonThatHasNothingToDoWithDenoSaysNothingOfIt(t *testing.T) {
	stubYTDLP(t, "unavailable")
	_, err := RunYTDLP(context.Background(), YTDLPOptions{
		URL: "https://example.test/watch?v=1", OutDir: t.TempDir(), Binary: "stub",
	})
	if err == nil || strings.Contains(err.Error(), "Deno") {
		t.Errorf("err = %v", err)
	}
}

// ---------- what the list shows ----------

func newYTDLPTask() *managedTask {
	mt := &managedTask{
		view: TaskView{ID: "yt", State: StateRunning, Kind: "yt-dlp", Size: -1},
		req:  jobRequest{URL: "https://example.test/watch?v=1", Kind: "yt-dlp"},
	}
	mt.gen = 1
	return mt
}

// A download can sit for seconds before yt-dlp says anything, and until then
// the list took it for a plain file: offered it a speed limit and a number of
// connections, neither of which does anything for yt-dlp.
func TestAYTDLPTaskIsOneFromTheStart(t *testing.T) {
	m := NewManager(t.TempDir(), 1)
	m.announce = nil
	m.mu.Lock()
	m.running = m.limit // the queue is full: it will not start
	m.mu.Unlock()
	id, err := m.Add(jobRequest{URL: "https://example.test/watch?v=1", Kind: "yt-dlp", Connections: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Pause(id)
	v, _ := findTask(m, id)
	if v.Kind != "yt-dlp" || v.Conns != 0 || v.ConnsWanted != 0 {
		t.Errorf("a queued yt-dlp download shows as kind %q with %d of %d connections", v.Kind, v.Conns, v.ConnsWanted)
	}

	// And it stays one across a restart: the list is read back with the number
	// of connections the request asked for.
	store := filepath.Join(t.TempDir(), "tasks.json")
	m.store = store
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	m2 := NewManager(t.TempDir(), 1)
	m2.store = store
	if err := m2.load(); err != nil {
		t.Fatal(err)
	}
	v, ok := findTask(m2, id)
	if !ok {
		t.Fatal("the task was not read back")
	}
	if v.Kind != "yt-dlp" || v.Conns != 0 || v.ConnsWanted != 0 {
		t.Errorf("after a restart it shows as kind %q with %d of %d connections", v.Kind, v.Conns, v.ConnsWanted)
	}
}

// The last line of a stream carries its speed over the whole stream. Nothing
// is arriving until the next one begins.
func TestAFinishedStreamHasNoSpeed(t *testing.T) {
	mt := newYTDLPTask()
	mt.ytProgress(1, YTProgress{Stage: "video", Status: "downloading", Downloaded: 500, Total: 1000, Speed: 900000})
	if v := mt.snapshot(); v.Speed != 900000 {
		t.Fatalf("speed = %d while downloading", v.Speed)
	}
	mt.ytProgress(1, YTProgress{Stage: "video", Status: "finished", Downloaded: 1000, Total: 1000, Speed: 700000})
	v := mt.snapshot()
	if v.Speed != 0 {
		t.Errorf("speed = %d once the stream is finished, want none", v.Speed)
	}
	if v.Received != 1000 || v.Size != 1000 {
		t.Errorf("the stream's size went with its speed: %d of %d", v.Received, v.Size)
	}
}

// yt-dlp says nothing while it looks the next stream up, or when the link has
// stalled, and what was on the screen stayed there for good.
func TestTheSpeedOfASilentYTDLPDownloadFallsToNothing(t *testing.T) {
	was := ytdlpSpeedStale
	ytdlpSpeedStale = 150 * time.Millisecond
	t.Cleanup(func() { ytdlpSpeedStale = was })

	mt := newYTDLPTask()
	mt.ytProgress(1, YTProgress{Stage: "video", Status: "downloading", Downloaded: 500, Total: 1000, Speed: 900000})
	if v := mt.snapshot(); v.Speed != 900000 {
		t.Fatalf("speed = %d right after progress", v.Speed)
	}
	time.Sleep(300 * time.Millisecond)
	if v := mt.snapshot(); v.Speed != 0 {
		t.Errorf("speed = %d after yt-dlp said nothing for a while, want none", v.Speed)
	}
	if v := mt.snapshot(); v.Received != 500 {
		t.Errorf("the bytes received went with the speed: %d", v.Received)
	}
	mt.ytProgress(1, YTProgress{Stage: "video", Status: "downloading", Downloaded: 600, Total: 1000, Speed: 800000})
	if v := mt.snapshot(); v.Speed != 800000 {
		t.Errorf("speed = %d once it went on", v.Speed)
	}
}

// The part of a big download in which nothing arrives and the bar is full.
func TestOnceEveryStreamIsInTheTaskSaysItIsPuttingThemTogether(t *testing.T) {
	mt := newYTDLPTask()
	mt.ytProgress(1, YTProgress{Stage: "audio", Status: "finished", Downloaded: 2000, Total: 2000, Speed: 5000})
	mt.ytProgress(1, YTProgress{Stage: ytProcessing, Status: "processing"})
	v := mt.snapshot()
	if v.Stage != ytProcessing {
		t.Errorf("stage = %q, want %q", v.Stage, ytProcessing)
	}
	if v.Speed != 0 || v.Received != 2000 || v.Size != 2000 {
		t.Errorf("speed %d, %d of %d: the bar is to stay where the last stream left it", v.Speed, v.Received, v.Size)
	}
}

// A run that begins again is some seconds in finding its way back to the
// download, and what the last one got to is not true of it: a card that says
// the streams are being put together while yt-dlp is still reading the page is
// wrong.
func TestAResumedYTDLPTaskForgetsWhereTheLastRunGotTo(t *testing.T) {
	stubYTDLP(t, "idler")
	beatFile(t)
	t.Setenv("GODM_YTDLP", os.Args[0])
	m := NewManager(t.TempDir(), 2)
	m.announce = nil
	id, err := m.Add(jobRequest{URL: "https://example.test/watch?v=1", Kind: "yt-dlp", Variant: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Remove(id, false) }) // which ends the stand-in
	waitFor(t, 30*time.Second, "the first run", stateIs(m, id, StateRunning))
	m.Pause(id)
	waitFor(t, 10*time.Second, "the pause", stateIs(m, id, StatePaused))

	mt := m.get(id)
	mt.mu.Lock()
	mt.view.Stage = ytProcessing // where that run got to
	mt.mu.Unlock()

	if !m.Resume(id) {
		t.Fatal("a paused task would not resume")
	}
	waitFor(t, 30*time.Second, "the second run", stateIs(m, id, StateRunning))
	if v, _ := findTask(m, id); v.Stage != "" {
		t.Errorf("stage = %q at the start of a run that has said nothing yet", v.Stage)
	}
}

// What the card shows for the stage has to be a word the page knows.
func TestThePageHasAWordForTheProcessingStage(t *testing.T) {
	for _, key := range []string{`"meta.processing": "putting it together"`, `"meta.processing": "正在合并处理"`} {
		if !strings.Contains(uiHTML, key) {
			t.Errorf("the page has no %s", key)
		}
	}
	if !strings.Contains(uiHTML, `t.stage === "processing"`) {
		t.Error("the page does not show the stage")
	}
}
