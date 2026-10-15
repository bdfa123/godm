package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// "Also delete the file" for a yt-dlp download that has not finished. Until it
// finishes the task has no file of its own to name: what is on disk is what
// yt-dlp keeps while it works, one file to a stream with ".part" added, a
// ".ytdl" beside it, the pieces of a stream that comes in fragments, and any
// stream that is already complete and waiting to be merged. yt-dlp does not
// say where they are: asked for a title and a file name with --print, it says
// nothing else.
//
// The stand-in in ytdlp_test.go writes no files, so these tests bring one that
// does: the test binary again, running the "test" below.

const partsEnv = "GODM_YTDLP_PARTS"

// What the stand-in leaves in the download folder at the point where it stops
// and waits to be ended, below the folder yt-dlp was given to keep it in.
var ytLeftovers = []string{
	"A Film.f396.mp4",                 // the picture, complete
	"A Film.f140.m4a.part",            // the sound, part way
	"A Film.f140.m4a.ytdl",            // yt-dlp's notes on it
	"A Film.f140.m4a.part-Frag7.part", // a fragment of it
}

// What was in the folder before and is nobody's to delete. Some of it is named
// like what yt-dlp leaves: the person who saved it never asked godm to guess
// at what a name means.
var ytBystanders = []string{
	"A Film.txt", "Another Film.mp4", "A Film.f140.m4a.parting-words.txt",
	"Another Film.f251.webm.part", "Another Film.f251.webm.ytdl",
}

// TestStandInForAnUnfinishedYTDLP is not a test. Started by the tests below in
// place of yt-dlp, it gets as far as the second stream of a video and stays
// there until it is ended. Like yt-dlp asked to --print, it says only what it
// was asked to say.
func TestStandInForAnUnfinishedYTDLP(t *testing.T) {
	if os.Getenv(partsEnv) == "" {
		return
	}
	home, temp := "", ""
	args := flag.Args() // what godm passed to yt-dlp
	for i := 0; i+1 < len(args) && args[i] != "--"; i++ {
		if args[i] == "-P" {
			if v, ok := strings.CutPrefix(args[i+1], "temp:"); ok {
				temp = v
			} else {
				home = strings.TrimPrefix(args[i+1], "home:")
			}
		}
	}
	if home == "" {
		fmt.Println("ERROR: no -P folder")
		os.Exit(2)
	}
	if temp == "" {
		temp = home // yt-dlp keeps it all in one place unless told otherwise
	}
	if err := os.MkdirAll(temp, 0o755); err != nil {
		fmt.Println("ERROR:", err)
		os.Exit(1)
	}
	write := func(name string) {
		if err := os.WriteFile(filepath.Join(temp, name), []byte("some of a video"), 0o600); err != nil {
			fmt.Println("ERROR:", err)
			os.Exit(1)
		}
	}
	video := filepath.Join(temp, "A Film.f396.mp4")

	fmt.Printf("%s%s\n", titleMarker, asciiJSON("A Film"))
	write("A Film.f396.mp4.part")
	fmt.Printf("%sdownloading|600|1000|NA|100|9|avc1.4d401f\n", progressMarker)
	// A stream takes a while to arrive.
	time.Sleep(500 * time.Millisecond)
	if err := os.Rename(video+".part", video); err != nil {
		fmt.Println("ERROR:", err)
		os.Exit(1)
	}
	fmt.Printf("%sfinished|1000|1000|NA|0|0|avc1.4d401f\n", progressMarker)

	write("A Film.f140.m4a.part")
	write("A Film.f140.m4a.ytdl")
	write("A Film.f140.m4a.part-Frag7.part")
	fmt.Printf("%sdownloading|77|500|NA|100|9|none\n", progressMarker)
	time.Sleep(time.Minute)
	os.Exit(0)
}

// filesUnder lists the files below dir, as paths relative to it.
func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// unfinishedYTDLP starts a yt-dlp download in dir and returns once the
// stand-in has left everything it is going to leave: with the files it left,
// which are every file in the folder that was not there before.
func unfinishedYTDLP(t *testing.T, dir string) (m *Manager, id string, made []string) {
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

	m = NewManager(dir, 2)
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
	for _, f := range filesUnder(t, dir) {
		if !slices.Contains(ytBystanders, f) {
			made = append(made, f)
		}
	}
	for _, name := range ytLeftovers {
		found := false
		for _, f := range made {
			found = found || filepath.Base(f) == name
		}
		if !found {
			t.Fatalf("the stand-in did not leave %s (the folder holds %q)", name, made)
		}
	}
	if v, _ := findTask(m, id); v.Path != "" {
		t.Fatalf("an unfinished yt-dlp download has the path %q; these tests are about one that has none", v.Path)
	}
	return m, id, made
}

// gone waits for yt-dlp's files to be deleted and checks nothing else was.
func gone(t *testing.T, dir string, made []string) {
	t.Helper()
	var left []string
	deadline := time.Now().Add(10 * time.Second)
	for {
		left = left[:0]
		for _, name := range made {
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
	if rest, _ := filepath.Glob(filepath.Join(dir, ".godm-*")); len(rest) > 0 {
		t.Errorf("the download's own folder is still there: %q", rest)
	}
	for _, name := range ytBystanders {
		if !exists(filepath.Join(dir, name)) {
			t.Errorf("%s was deleted, and it was never the download's", name)
		}
	}
}

func TestRemovingAnUnfinishedYTDLPDownloadWithItsFiles(t *testing.T) {
	t.Run("while it runs", func(t *testing.T) {
		dir := t.TempDir()
		m, id, made := unfinishedYTDLP(t, dir)
		if !m.Remove(id, true) {
			t.Fatal("the download could not be removed")
		}
		gone(t, dir, made)
	})
	t.Run("paused", func(t *testing.T) {
		dir := t.TempDir()
		m, id, made := unfinishedYTDLP(t, dir)
		m.Pause(id)
		waitFor(t, 10*time.Second, "the pause", stateIs(m, id, StatePaused))
		if !m.Remove(id, true) {
			t.Fatal("the download could not be removed")
		}
		gone(t, dir, made)
	})
	t.Run("paused, after a restart", func(t *testing.T) {
		dir := t.TempDir()
		store := filepath.Join(t.TempDir(), "tasks.json")
		m, id, made := unfinishedYTDLP(t, dir)
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
		gone(t, dir, made)
	})
	t.Run("the files were not asked for", func(t *testing.T) {
		dir := t.TempDir()
		m, id, made := unfinishedYTDLP(t, dir)
		m.Pause(id)
		waitFor(t, 10*time.Second, "the pause", stateIs(m, id, StatePaused))
		if !m.Remove(id, false) {
			t.Fatal("the download could not be removed")
		}
		time.Sleep(300 * time.Millisecond)
		for _, name := range append(slices.Clone(made), ytBystanders...) {
			if !exists(filepath.Join(dir, name)) {
				t.Errorf("%s was deleted, and only the download was to go", name)
			}
		}
	})
}

// Pausing is not removing: what is part way stays, for yt-dlp to carry on from.
func TestPausingAYTDLPDownloadKeepsItsParts(t *testing.T) {
	dir := t.TempDir()
	m, id, made := unfinishedYTDLP(t, dir)
	m.Pause(id)
	waitFor(t, 10*time.Second, "the pause", stateIs(m, id, StatePaused))
	time.Sleep(300 * time.Millisecond)
	for _, name := range made {
		if !exists(filepath.Join(dir, name)) {
			t.Errorf("%s was deleted by a pause", name)
		}
	}
}
