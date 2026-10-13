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
	opts, err := readStandInOptions(args)
	if err != nil {
		say("ERROR: %v", err)
		return 2
	}
	dir, title := opts.home, opts.title
	if dir == "" {
		dir = "."
	}

	switch scene {
	case "ok":
		if contains(args, "-J") {
			say(`{"title": %q, "duration": 60, "formats": [`+
				`{"format_id": "18", "ext": "mp4", "height": 720, "tbr": 1500, "vcodec": "avc1"}]}`, title)
			break
		}
		say("[youtube] Extracting URL: https://example.test/watch")
		say("[download] Destination: %s", filepath.Join(dir, title+".f396.mp4"))
		progress("downloading", 1<<20, 8<<20, 500000, 14, "avc1.4d401f")
		progress("downloading", 4<<20, 8<<20, 900000, 4, "avc1.4d401f")
		progress("finished", 8<<20, 8<<20, 0, 0, "avc1.4d401f")
		progress("downloading", 1<<19, 2<<20, 400000, 3, "none")
		progress("finished", 2<<20, 2<<20, 0, 0, "none")
		say("[Merger] Merging formats into \"%s\"", filepath.Join(dir, title+".mp4"))
		say("%s%s", fileMarker, filepath.Join(dir, title+".mp4"))
	case "unavailable":
		say("[youtube] Extracting URL: https://example.test/watch")
		say("ERROR: [youtube] xyz: Video unavailable. This video is private")
		return 1
	case "silent":
		progress("downloading", 1<<20, 4<<20, 100000, 30, "avc1")
	case "where":
		// The folder it was started in, reported as if it were the file.
		wd, _ := os.Getwd()
		say("%s%s", fileMarker, wd)
	}
	return 0
}

// standInOptions is what the stand-in makes of its command line and of the
// configuration files that go with it.
type standInOptions struct {
	home  string // the -P folder
	title string // the file's name without its extension, from -o
}

// readStandInOptions follows the CONFIGURATION section of yt-dlp's README as
// far as these tests need it: a file given to --config-locations is always
// read and has to exist; unless --ignore-config is given, so is a yt-dlp.conf
// beside the binary (the "portable" configuration) and one in the folder given
// to -P or, without -P, in the current folder (the "home" configuration). The
// user's own configuration is left out of the searched places, or what the
// tests saw would depend on the machine they ran on. A configuration file is
// one option to a line, and the command line wins over it; -o is the only
// option the stand-in obeys from one, which is enough to see whether a file
// was read.
func readStandInOptions(args []string) (standInOptions, error) {
	var o standInOptions
	var files []string
	var template string
	ignore := false
	for i := 0; i < len(args) && args[i] != "--"; i++ {
		value := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch args[i] {
		case "-P":
			o.home = strings.TrimPrefix(value(), "home:")
		case "-o":
			template = value()
		case "--ignore-config", "--no-config":
			ignore = true
		case "--config-locations":
			file := value()
			if _, err := os.Stat(file); err != nil {
				return o, fmt.Errorf("config location %s does not exist", file)
			}
			files = append(files, file)
		}
	}
	if !ignore {
		if exe, err := os.Executable(); err == nil {
			files = append(files, filepath.Join(filepath.Dir(exe), "yt-dlp.conf"))
		}
		files = append(files, filepath.Join(o.home, "yt-dlp.conf"))
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "-o "); ok && template == "" {
				template = strings.Trim(strings.TrimSpace(v), `"`)
			}
		}
	}
	if template == "" {
		template = "A Video Title.%(ext)s"
	}
	o.title = strings.TrimSuffix(template, ".%(ext)s")
	return o, nil
}

// emptyProfile gives the test a user profile with no yt-dlp configuration in
// it and returns its folder. Whatever the person running the tests keeps in
// their own is theirs, and must not decide what a test sees.
func emptyProfile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("APPDATA", filepath.Join(dir, "AppData", "Roaming"))
	t.Setenv("XDG_CONFIG_HOME", "")
	return dir
}

// stubYTDLP points the runner at this test binary for the duration of a test,
// in a profile of its own, whose folder it returns.
func stubYTDLP(t *testing.T, scene string) string {
	t.Helper()
	profile := emptyProfile(t)
	restore := newYTDLPCmd
	newYTDLPCmd = func(ctx context.Context, bin string, args []string) *exec.Cmd {
		c := exec.CommandContext(ctx, os.Args[0], args...)
		c.Env = append(os.Environ(), stubEnv+"="+scene)
		return c
	}
	t.Cleanup(func() { newYTDLPCmd = restore })
	return profile
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

// ---------- whose configuration it reads ----------

// plantedConf is what these tests leave where a download could have put it.
// It only renames the file, which is all it takes to show it was read; a real
// one could as easily carry --exec.
const plantedConf = "-o \"CONFIG-WAS-READ.%(ext)s\"\n"

func plantConf(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "yt-dlp.conf"), []byte(plantedConf), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAFileInTheDownloadFolderIsNeverYTDLPConfiguration(t *testing.T) {
	stubYTDLP(t, "ok")
	// The download folder is where files named by servers land, and
	// yt-dlp.conf is a name a server is free to choose.
	dir := t.TempDir()
	plantConf(t, dir)
	path, err := RunYTDLP(context.Background(), YTDLPOptions{
		URL: "https://example.test/watch?v=1", OutDir: dir, Binary: "stub",
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "A Video Title.mp4"); path != want {
		t.Errorf("path = %q, want %q: the yt-dlp.conf in the download folder was read as options", path, want)
	}
}

func TestInspectingReadsNoConfigurationFromTheCurrentFolder(t *testing.T) {
	stubYTDLP(t, "ok")
	// godm's current folder is wherever it happened to be started, and started
	// by a double click that is the folder it was downloaded to.
	dir := t.TempDir()
	plantConf(t, dir)
	t.Chdir(dir)
	info, err := InspectYTDLP(context.Background(), YTDLPOptions{
		URL: "https://example.test/watch?v=1", Binary: "stub",
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != "A Video Title" {
		t.Errorf("title = %q: the yt-dlp.conf in the current folder was read as options", info.Title)
	}
	if len(info.Variants) != 1 || info.Variants[0].Height != 720 {
		t.Errorf("variants = %+v, want the one 720p picture", info.Variants)
	}
}

// The advice for someone without yt-dlp is to put it next to godm.exe, and
// godm.exe is often run from the folder it was downloaded to. A yt-dlp.conf
// beside the binary is then a file in the download folder like any other.
func TestAFileBesideYTDLPIsNotConfigurationEither(t *testing.T) {
	stubYTDLP(t, "ok")
	exe, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	beside := filepath.Dir(exe)
	if _, err := os.Stat(filepath.Join(beside, "yt-dlp.conf")); err == nil {
		t.Skip("there is already a yt-dlp.conf beside the test binary, and it is not ours to replace")
	}
	plantConf(t, beside)
	t.Cleanup(func() { os.Remove(filepath.Join(beside, "yt-dlp.conf")) })

	dir := t.TempDir()
	path, err := RunYTDLP(context.Background(), YTDLPOptions{
		URL: "https://example.test/watch?v=1", OutDir: dir, Binary: "stub",
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "A Video Title.mp4"); path != want {
		t.Errorf("path = %q, want %q: the yt-dlp.conf beside the binary was read as options", path, want)
	}
	info, err := InspectYTDLP(context.Background(), YTDLPOptions{
		URL: "https://example.test/watch?v=1", Binary: "stub",
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != "A Video Title" {
		t.Errorf("title = %q: the yt-dlp.conf beside the binary was read as options", info.Title)
	}
}

// Shutting out the folders a download can reach must not shut out the person
// whose yt-dlp it is: their cookies, proxy and naming live in their own file.
func TestTheUsersOwnYTDLPConfigurationStillApplies(t *testing.T) {
	profile := stubYTDLP(t, "ok")
	conf := filepath.Join(profile, "AppData", "Roaming", "yt-dlp", "config")
	if err := os.MkdirAll(filepath.Dir(conf), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conf, []byte("-o \"Named By Me.%(ext)s\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path, err := RunYTDLP(context.Background(), YTDLPOptions{
		URL: "https://example.test/watch?v=1", OutDir: dir, Binary: "stub",
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "Named By Me.mp4"); path != want {
		t.Errorf("path = %q, want %q: the user's own configuration was not read", path, want)
	}
	info, err := InspectYTDLP(context.Background(), YTDLPOptions{
		URL: "https://example.test/watch?v=1", Binary: "stub",
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != "Named By Me" {
		t.Errorf("title = %q: the user's own configuration was not read", info.Title)
	}
}

// With no file of the user's there is nothing to hand over, and naming one
// that does not exist would stop yt-dlp before it started.
func TestYTDLPIsToldToSearchNowhereForConfiguration(t *testing.T) {
	emptyProfile(t)
	for what, args := range map[string][]string{
		"a download":    ytdlpArgs(YTDLPOptions{URL: "https://example.test/watch?v=1", OutDir: "out"}),
		"an inspection": ytdlpConfigArgs(),
	} {
		if !contains(args, "--ignore-config") {
			t.Errorf("%s: --ignore-config is missing from %v", what, args)
		}
		if contains(args, "--config-locations") {
			t.Errorf("%s: %v names a configuration file, and there is none", what, args)
		}
	}
}

func TestTheUsersYTDLPConfigurationIsFoundWhereYTDLPLooks(t *testing.T) {
	// The order is the one in yt-dlp's README under "User Configuration".
	// Each case writes its file and every file that comes after it, so a
	// wrong order shows as the wrong file being picked.
	type place struct{ root, rel string }
	order := []place{
		{"xdg", "yt-dlp.conf"},
		{"xdg", "yt-dlp/config"},
		{"xdg", "yt-dlp/config.txt"},
		{"appdata", "yt-dlp.conf"},
		{"appdata", "yt-dlp/config"},
		{"appdata", "yt-dlp/config.txt"},
		{"home", "yt-dlp.conf"},
		{"home", "yt-dlp.conf.txt"},
		{"home", ".yt-dlp/config"},
		{"home", ".yt-dlp/config.txt"},
	}
	for first := range order {
		base := t.TempDir()
		roots := map[string]string{
			"xdg":     filepath.Join(base, "xdg"),
			"appdata": filepath.Join(base, "appdata"),
			"home":    filepath.Join(base, "home"),
		}
		t.Setenv("XDG_CONFIG_HOME", roots["xdg"])
		t.Setenv("APPDATA", roots["appdata"])
		t.Setenv("HOME", roots["home"])
		t.Setenv("USERPROFILE", roots["home"])
		file := func(p place) string { return filepath.Join(roots[p.root], filepath.FromSlash(p.rel)) }
		for _, p := range order[first:] {
			if err := os.MkdirAll(filepath.Dir(file(p)), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file(p), []byte("--no-mtime\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if got, want := ytdlpUserConfig(), file(order[first]); got != want {
			t.Errorf("with %s/%s the first file there, picked %q, want %q",
				order[first].root, order[first].rel, got, want)
		}
	}

	// Without XDG_CONFIG_HOME that place is .config in the home folder.
	home := emptyProfile(t)
	conf := filepath.Join(home, ".config", "yt-dlp", "config")
	if err := os.MkdirAll(filepath.Dir(conf), 0o700); err != nil {
		t.Fatal(err)
	}
	// A folder by a configuration file's name is not one.
	if err := os.MkdirAll(filepath.Join(home, ".config", "yt-dlp.conf"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := ytdlpUserConfig(); got != "" {
		t.Errorf("picked %q with no configuration file anywhere", got)
	}
	if err := os.WriteFile(conf, []byte("--no-mtime\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ytdlpUserConfig(); got != conf {
		t.Errorf("picked %q, want %q", got, conf)
	}
	args := ytdlpConfigArgs()
	if len(args) != 3 || args[0] != "--ignore-config" || args[1] != "--config-locations" || args[2] != conf {
		t.Errorf("args = %q, want the search turned off and then the user's file", args)
	}
}

// godm's current folder is no better a place than its download folder: a
// script wrapped around yt-dlp, or the Python under one, loads what is there.
func TestYTDLPIsStartedInItsOwnFolder(t *testing.T) {
	stubYTDLP(t, "where")
	t.Chdir(t.TempDir())
	started, err := RunYTDLP(context.Background(), YTDLPOptions{
		URL: "https://example.test/watch?v=1", OutDir: t.TempDir(), Binary: "stub",
	})
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	if !sameFolder(started, filepath.Dir(exe)) {
		t.Errorf("started in %q, want the folder the stand-in is in, %q", started, filepath.Dir(exe))
	}
}

// A download folder named from godm's current folder must go on meaning that
// folder once yt-dlp is started somewhere else.
func TestARelativeDownloadFolderIsStillGodms(t *testing.T) {
	stubYTDLP(t, "ok")
	here := t.TempDir()
	t.Chdir(here)
	path, err := RunYTDLP(context.Background(), YTDLPOptions{
		URL: "https://example.test/watch?v=1", OutDir: "videos", Binary: "stub",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(path) || !sameFolder(filepath.Dir(path), filepath.Join(here, "videos")) {
		t.Errorf("path = %q, want the file in %q", path, filepath.Join(here, "videos"))
	}
}

// sameFolder compares two spellings of what may be one folder.
func sameFolder(a, b string) bool {
	fa, errA := os.Stat(a)
	fb, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(fa, fb)
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
