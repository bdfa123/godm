package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// "Also delete the file" for a yt-dlp download that has not finished. Until it
// finishes the task has no file of its own to name: what is on disk is what
// yt-dlp keeps while it works, one file to a stream with ".part" added, a
// ".ytdl" beside it, the pieces of a stream that comes in fragments, and any
// stream that is already complete and waiting to be merged.
//
// The stand-in in ytdlp_test.go writes no files, so these tests bring one that
// does: the test binary again, running the "test" below.

const partsEnv = "GODM_YTDLP_PARTS"

// What the stand-in leaves in the download folder at the point where it stops
// and waits to be ended.
var ytLeftovers = []string{
	"A Film.f396.mp4",                 // the picture, complete
	"A Film.f140.m4a.part",            // the sound, part way
	"A Film.f140.m4a.ytdl",            // yt-dlp's notes on it
	"A Film.f140.m4a.part-Frag7.part", // a fragment of it
}

// What was in the folder before and is nobody's to delete.
var ytBystanders = []string{"A Film.txt", "Another Film.mp4", "A Film.f140.m4a.parting-words.txt"}

// TestStandInForAnUnfinishedYTDLP is not a test. Started by the tests below in
// place of yt-dlp, it gets as far as the second stream of a video and stays
// there until it is ended.
func TestStandInForAnUnfinishedYTDLP(t *testing.T) {
	if os.Getenv(partsEnv) == "" {
		return
	}
	dir := ""
	args := flag.Args() // what godm passed to yt-dlp
	for i := 0; i+1 < len(args) && args[i] != "--"; i++ {
		if args[i] == "-P" {
			dir = args[i+1]
		}
	}
	if dir == "" {
		fmt.Println("ERROR: no -P folder")
		os.Exit(2)
	}
	write := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("some of a video"), 0o600); err != nil {
			fmt.Println("ERROR:", err)
			os.Exit(1)
		}
	}
	video, audio := filepath.Join(dir, "A Film.f396.mp4"), filepath.Join(dir, "A Film.f140.m4a")

	fmt.Println("[youtube] Extracting URL: https://example.test/watch")
	fmt.Printf("[download] Destination: %s\n", video)
	write("A Film.f396.mp4.part")
	fmt.Printf("%sdownloading|600|1000|NA|100|9|avc1.4d401f\n", progressMarker)
	// A stream takes a while to arrive, the announcement of it none.
	time.Sleep(500 * time.Millisecond)
	if err := os.Rename(video+".part", video); err != nil {
		fmt.Println("ERROR:", err)
		os.Exit(1)
	}
	fmt.Printf("%sfinished|1000|1000|NA|0|0|avc1.4d401f\n", progressMarker)

	fmt.Printf("[download] Destination: %s\n", audio)
	write("A Film.f140.m4a.part")
	write("A Film.f140.m4a.ytdl")
	write("A Film.f140.m4a.part-Frag7.part")
	fmt.Printf("%sdownloading|77|500|NA|100|9|none\n", progressMarker)
	time.Sleep(time.Minute)
	os.Exit(0)
}

// unfinishedYTDLP starts a yt-dlp download in dir and returns once the
// stand-in has left everything it is going to leave.
func unfinishedYTDLP(t *testing.T, dir string) (*Manager, string) {
	t.Helper()
	emptyProfile(t)
	restore := newYTDLPCmd
	newYTDLPCmd = func(ctx context.Context, bin string, args []string) *exec.Cmd {
		c := exec.CommandContext(ctx, os.Args[0],
			append([]string{"-test.run=^TestStandInForAnUnfinishedYTDLP$", "--"}, args...)...)
		c.Env = append(os.Environ(), partsEnv+"=1")
		return c
	}
	t.Cleanup(func() { newYTDLPCmd = restore })
	t.Setenv("GODM_YTDLP", os.Args[0])
	for _, name := range ytBystanders {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not yt-dlp's"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	m := NewManager(dir, 2)
	m.announce = nil
	id, err := m.Add(jobRequest{URL: "https://example.test/watch?v=1", Kind: "yt-dlp", Variant: -1, OutDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Remove(id, false) }) // which ends the stand-in
	waitFor(t, 30*time.Second, "yt-dlp to be part way through the second stream", func() bool {
		v, _ := findTask(m, id)
		return v.State == StateRunning && v.Received == 77
	})
	for _, name := range ytLeftovers {
		if !exists(filepath.Join(dir, name)) {
			t.Fatalf("the stand-in did not leave %s", name)
		}
	}
	if v, _ := findTask(m, id); v.Path != "" {
		t.Fatalf("an unfinished yt-dlp download has the path %q; these tests are about one that has none", v.Path)
	}
	return m, id
}

// gone waits for yt-dlp's files to be deleted and checks nothing else was.
func gone(t *testing.T, dir string) {
	t.Helper()
	var left []string
	deadline := time.Now().Add(10 * time.Second)
	for {
		left = left[:0]
		for _, name := range ytLeftovers {
			if exists(filepath.Join(dir, name)) {
				left = append(left, name)
			}
		}
		if len(left) == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(left) > 0 {
		t.Errorf("the download was removed along with its files, and these are still in the folder: %q", left)
	}
	for _, name := range ytBystanders {
		if !exists(filepath.Join(dir, name)) {
			t.Errorf("%s was deleted, and it was never the download's", name)
		}
	}
}

// What is remembered for deleting is a name read out of yt-dlp's output, and
// part of what yt-dlp prints is the site's words. A name that is not a file
// this download is making must never be remembered.
func TestOnlyFilesTheDownloadIsMakingAreRememberedForDeleting(t *testing.T) {
	dir := t.TempDir()
	finished := filepath.Join(dir, "Already Here.mp4")
	if err := os.WriteFile(finished, []byte("somebody's film"), 0o600); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(dir, "A Film.f396.mp4")
	for _, c := range []struct {
		name, file, want string
	}{
		{"a stream about to be written", inside, inside},
		{"in a folder of the download folder", filepath.Join(dir, "Channel", "A Film.mp4"), filepath.Join(dir, "Channel", "A Film.mp4")},
		{"a file that is already there", finished, ""},
		{"outside the folder", filepath.Join(filepath.Dir(dir), "elsewhere.mp4"), ""},
		{"outside by way of ..", filepath.Join(dir, "..", "elsewhere.mp4"), ""},
		{"the folder itself", dir, ""},
		{"not spelled out in full", "A Film.f396.mp4", ""},
		{"nothing", "", ""},
	} {
		if got := ytOwnFile(dir, c.file); got != c.want {
			t.Errorf("%s: %q is remembered as %q, want %q", c.name, c.file, got, c.want)
		}
	}

	// And only yt-dlp's own announcement names a file at all.
	for line, want := range map[string]string{
		"[download] Destination: " + inside:                            inside,
		"[download] Downloading playlist: x] Destination: " + finished: "",
		"[ExtractAudio] Destination: " + inside:                        "",
		"[download]  12.0% of 3.00MiB":                                 "",
	} {
		if got := ytdlpDestination(line); got != want {
			t.Errorf("%q names the file %q, want %q", line, got, want)
		}
	}
}

// Once a download is done yt-dlp has cleared up after itself, and what is left
// is the one file at the task's path. Streams remembered from before are no
// longer the task's to delete: under their names there may by then be files
// the user kept on purpose.
func TestAFinishedYTDLPDownloadForgetsItsStreams(t *testing.T) {
	dir := t.TempDir()
	done := filepath.Join(dir, "A Film.mp4")
	kept := filepath.Join(dir, "A Film.f396.mp4")
	for _, p := range []string{done, kept} {
		if err := os.WriteFile(p, []byte("video"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m := NewManager(dir, 1)
	m.announce = nil
	mt := &managedTask{
		view:    TaskView{ID: "yt", State: StateRunning, Kind: "yt-dlp"},
		req:     jobRequest{URL: "https://example.test/watch?v=1", Kind: "yt-dlp"},
		outDir:  dir,
		ytFiles: []string{kept},
	}
	mt.gen = 1
	m.mu.Lock()
	m.tasks["yt"] = mt
	m.order = append(m.order, "yt")
	m.mu.Unlock()

	m.finishRun(mt, 1, StateDone, "", done)
	if !m.Remove("yt", true) {
		t.Fatal("the download could not be removed")
	}
	waitFor(t, 5*time.Second, "the finished file to be deleted", func() bool { return !exists(done) })
	if !exists(kept) {
		t.Error("a file that yt-dlp left on purpose was deleted with the finished download")
	}
}

func TestRemovingAnUnfinishedYTDLPDownloadWithItsFiles(t *testing.T) {
	t.Run("while it runs", func(t *testing.T) {
		dir := t.TempDir()
		m, id := unfinishedYTDLP(t, dir)
		if !m.Remove(id, true) {
			t.Fatal("the download could not be removed")
		}
		gone(t, dir)
	})
	t.Run("paused", func(t *testing.T) {
		dir := t.TempDir()
		m, id := unfinishedYTDLP(t, dir)
		m.Pause(id)
		waitFor(t, 10*time.Second, "the pause", stateIs(m, id, StatePaused))
		if !m.Remove(id, true) {
			t.Fatal("the download could not be removed")
		}
		gone(t, dir)
	})
	t.Run("paused, after a restart", func(t *testing.T) {
		dir := t.TempDir()
		store := filepath.Join(t.TempDir(), "tasks.json")
		m, id := unfinishedYTDLP(t, dir)
		m.store = store
		m.Pause(id)
		waitFor(t, 10*time.Second, "the pause", stateIs(m, id, StatePaused))
		if err := m.save(); err != nil {
			t.Fatal(err)
		}
		m2 := NewManager(dir, 2)
		m2.announce = nil
		m2.store = store
		if err := m2.load(); err != nil {
			t.Fatal(err)
		}
		if !m2.Remove(id, true) {
			t.Fatal("the download could not be removed")
		}
		gone(t, dir)
	})
	t.Run("the files were not asked for", func(t *testing.T) {
		dir := t.TempDir()
		m, id := unfinishedYTDLP(t, dir)
		m.Pause(id)
		waitFor(t, 10*time.Second, "the pause", stateIs(m, id, StatePaused))
		if !m.Remove(id, false) {
			t.Fatal("the download could not be removed")
		}
		time.Sleep(300 * time.Millisecond)
		for _, name := range append(ytLeftovers, ytBystanders...) {
			if !exists(filepath.Join(dir, name)) {
				t.Errorf("%s was deleted, and only the download was to go", name)
			}
		}
	})
}
