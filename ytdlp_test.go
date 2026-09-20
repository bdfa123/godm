package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// yt-dlp is not installed on every machine that runs these tests, and the ones
// where it is would reach the real internet. The test binary stands in for it:
// TestMain notices the marker in the environment and behaves like yt-dlp
// instead of running tests, which it can only do before any test output has
// been printed.

const stubEnv = "GODM_YTDLP_STUB"

func TestMain(m *testing.M) {
	if scene := os.Getenv(stubEnv); scene != "" {
		os.Exit(playYTDLP(scene, os.Args[1:]))
	}
	os.Exit(m.Run())
}

func playYTDLP(scene string, args []string) int {
	out := os.Stdout
	say := func(format string, a ...any) { fmt.Fprintf(out, format+"\n", a...) }
	progress := func(status string, done, total, speed, eta int64, vcodec string) {
		say("%s%s|%d|%d|NA|%d|%d|%s", progressMarker, status, done, total, speed, eta, vcodec)
	}
	// Whatever -P was given is where the file is claimed to land, so the test
	// can check the path came back through unharmed.
	dir := "."
	for i, a := range args {
		if a == "-P" && i+1 < len(args) {
			dir = args[i+1]
		}
	}

	switch scene {
	case "ok":
		say("[youtube] Extracting URL: https://example.test/watch")
		say("[download] Destination: %s", filepath.Join(dir, "A Video Title.f396.mp4"))
		progress("downloading", 1<<20, 8<<20, 500000, 14, "avc1.4d401f")
		progress("downloading", 4<<20, 8<<20, 900000, 4, "avc1.4d401f")
		progress("finished", 8<<20, 8<<20, 0, 0, "avc1.4d401f")
		progress("downloading", 1<<19, 2<<20, 400000, 3, "none")
		progress("finished", 2<<20, 2<<20, 0, 0, "none")
		say("[Merger] Merging formats into \"%s\"", filepath.Join(dir, "A Video Title.mp4"))
		say("%s%s", fileMarker, filepath.Join(dir, "A Video Title.mp4"))
	case "unavailable":
		say("[youtube] Extracting URL: https://example.test/watch")
		say("ERROR: [youtube] xyz: Video unavailable. This video is private")
		return 1
	case "silent":
		progress("downloading", 1<<20, 4<<20, 100000, 30, "avc1")
	}
	return 0
}

// stubYTDLP points the runner at this test binary for the duration of a test.
func stubYTDLP(t *testing.T, scene string) {
	t.Helper()
	restore := newYTDLPCmd
	newYTDLPCmd = func(ctx context.Context, bin string, args []string) *exec.Cmd {
		c := exec.CommandContext(ctx, os.Args[0], args...)
		c.Env = append(os.Environ(), stubEnv+"="+scene)
		return c
	}
	t.Cleanup(func() { newYTDLPCmd = restore })
}

// ---------- reading what yt-dlp says ----------

func TestProgressLinesAreRead(t *testing.T) {
	cases := []struct {
		name string
		line string
		want YTProgress
		ok   bool
	}{
		{
			name: "a normal update",
			line: progressMarker + "downloading|1048576|8388608|NA|524288.0|14|avc1.4d401f",
			want: YTProgress{Stage: "video", Status: "downloading",
				Downloaded: 1048576, Total: 8388608, Speed: 524288, ETA: 14},
			ok: true,
		},
		{
			name: "a stream with no picture is the audio half",
			line: progressMarker + "downloading|1024|2048|NA|100|3|none",
			want: YTProgress{Stage: "audio", Status: "downloading",
				Downloaded: 1024, Total: 2048, Speed: 100, ETA: 3},
			ok: true,
		},
		{
			// Everything yt-dlp has not worked out yet comes through as NA.
			name: "unknowns become zero, and the estimate stands in for the total",
			line: progressMarker + "downloading|500|NA|4096|NA|NA|NA",
			want: YTProgress{Status: "downloading", Downloaded: 500, Total: 4096},
			ok:   true,
		},
		{name: "an ordinary log line", line: "[download] 12.3% of 4.00MiB"},
		{name: "a truncated line", line: progressMarker + "downloading|1|2"},
		{name: "something else entirely", line: "ERROR: nope"},
	}
	for _, c := range cases {
		got, ok := parseYTProgress(c.line)
		if ok != c.ok {
			t.Errorf("%s: recognised = %v, want %v", c.name, ok, c.ok)
			continue
		}
		if ok && got != c.want {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}
}

func TestTheURLCanNeverBeReadAsAFlag(t *testing.T) {
	args := ytdlpArgs(YTDLPOptions{URL: "--config-location=/tmp/evil", OutDir: "out"})
	last := args[len(args)-2:]
	if last[0] != "--" {
		t.Fatalf("args end with %v; without a -- separator a URL starting with a dash "+
			"becomes an option to yt-dlp", last)
	}
	if last[1] != "--config-location=/tmp/evil" {
		t.Errorf("the URL was altered: %q", last[1])
	}
	// A page URL carrying a playlist must fetch one video, not two hundred.
	if !contains(args, "--no-playlist") {
		t.Error("--no-playlist is missing")
	}
	if !contains(args, "--no-overwrites") {
		t.Error("--no-overwrites is missing; godm never writes over a file that is there")
	}
}

func contains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// ---------- driving the process ----------

func TestRunYTDLPReportsBothHalvesAndTheFinalFile(t *testing.T) {
	stubYTDLP(t, "ok")
	dir := t.TempDir()

	var mu sync.Mutex
	var stages []string
	var title string
	path, err := RunYTDLP(context.Background(), YTDLPOptions{
		URL: "https://example.test/watch?v=1", OutDir: dir, Binary: "stub",
		OnStart: func(s YTStart) {
			mu.Lock()
			title = s.Title
			mu.Unlock()
		},
		OnUpdate: func(p YTProgress) {
			mu.Lock()
			stages = append(stages, p.Stage+":"+p.Status)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "A Video Title.mp4"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if title != "A Video Title.f396" {
		t.Errorf("title = %q", title)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{
		"video:downloading", "video:downloading", "video:finished",
		"audio:downloading", "audio:finished",
	}
	if strings.Join(stages, " ") != strings.Join(want, " ") {
		t.Errorf("stages =\n %v\nwant\n %v", stages, want)
	}
}

func TestRunYTDLPPassesOnTheReasonItFailed(t *testing.T) {
	stubYTDLP(t, "unavailable")
	_, err := RunYTDLP(context.Background(), YTDLPOptions{
		URL: "https://example.test/watch?v=1", OutDir: t.TempDir(), Binary: "stub",
	})
	if err == nil {
		t.Fatal("expected a failure")
	}
	if !strings.Contains(err.Error(), "Video unavailable") {
		t.Errorf("err = %v; the ERROR line is the one worth showing, not the exit code", err)
	}
}

func TestRunYTDLPWillNotClaimSuccessWithoutAFile(t *testing.T) {
	stubYTDLP(t, "silent")
	_, err := RunYTDLP(context.Background(), YTDLPOptions{
		URL: "https://example.test/watch?v=1", OutDir: t.TempDir(), Binary: "stub",
	})
	if err == nil {
		t.Fatal("it exited zero without saying where the file went; that is not a success")
	}
}

func TestMissingYTDLPSaysHowToGetIt(t *testing.T) {
	err := (&YTDLPMissingError{}).Error()
	for _, want := range []string{"yt-dlp", "winget", "next to godm.exe"} {
		if !strings.Contains(err, want) {
			t.Errorf("the message leaves out %q: %s", want, err)
		}
	}
}

func TestFormatSelectorAsksForAHeightNotAFormatID(t *testing.T) {
	// Format ids move around; a height means the same thing every day, and the
	// fallbacks keep it working when only one combined file exists.
	got := ytdlpFormatFor(1080)
	for _, want := range []string{"height<=1080", "+ba", "/b"} {
		if !strings.Contains(got, want) {
			t.Errorf("selector %q is missing %q", got, want)
		}
	}
	if ytdlpFormatFor(0) != "" {
		t.Error("no chosen height should leave the choice to yt-dlp")
	}
}
