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
