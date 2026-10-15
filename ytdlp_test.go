package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
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

	if scene == "longline" {
		// A line longer than anything a reader keeps whole, then more than a
		// pipe holds, before going on as a download that works.
		say("[debug] %s", strings.Repeat("x", 3<<20))
		for i := range 4000 {
			say("[debug] line %d %s", i, strings.Repeat("y", 100))
		}
		scene = "ok"
	}

	switch scene {
	case "tree":
		// yt-dlp.exe is a launcher that runs the real program as its child,
		// and ffmpeg is a child of that: what does the work is never the
		// process godm started.
		worker, err := startStandIn("worker")
		if err != nil {
			say("ERROR: %v", err)
			return 1
		}
		worker.Wait()
	case "worker":
		return standInLingers(func(i int) {
			progress("downloading", int64(i)*1000, 200000, 1000, 9, "avc1.4d401f")
		})
	case "idler":
		return standInLingers(nil)
	case "godm":
		// Not yt-dlp this time but godm with a download under way, for the
		// test that wants to see what becomes of the download when godm dies.
		newYTDLPCmd = func(ctx context.Context, bin string, args []string) *exec.Cmd {
			c := exec.CommandContext(ctx, os.Args[0], args...)
			c.Env = append(os.Environ(), stubEnv+"=tree")
			return c
		}
		RunYTDLP(context.Background(), YTDLPOptions{
			URL: "https://example.test/watch?v=1", OutDir: filepath.Dir(os.Getenv(stubBeatEnv)), Binary: "stub",
		})
	case "leftover":
		// Something started and not waited for, a player from the user's own
		// --exec say. It holds the output open without ever writing to it.
		if _, err := startStandIn("idler"); err != nil {
			say("ERROR: %v", err)
			return 1
		}
		fallthrough
	case "ok":
		if contains(args, "-J") {
			say(`{"title": %q, "duration": 60, "formats": [`+
				`{"format_id": "18", "ext": "mp4", "height": 720, "tbr": 1500, "vcodec": "avc1"}]}`, title)
			break
		}
		// --print implies --quiet, and then yt-dlp leaves out what it would say
		// about itself, the Destination line with the rest, unless it is told
		// not to be quiet. What is printed is only what was asked for.
		quiet := !contains(args, "--no-quiet")
		if !quiet {
			say("[youtube] Extracting URL: https://example.test/watch")
		}
		if asked(args, "before_dl:"+titleMarker+"%(title)j") {
			say("%s%s", titleMarker, asciiJSON(title))
		}
		if !quiet {
			say("[download] Destination: %s", filepath.Join(dir, title+".f396.mp4"))
		}
		progress("downloading", 1<<20, 8<<20, 500000, 14, "avc1.4d401f")
		progress("downloading", 4<<20, 8<<20, 900000, 4, "avc1.4d401f")
		progress("finished", 8<<20, 8<<20, 0, 0, "avc1.4d401f")
		progress("downloading", 1<<19, 2<<20, 400000, 3, "none")
		progress("finished", 2<<20, 2<<20, 0, 0, "none")
		if !quiet {
			say("[Merger] Merging formats into \"%s\"", filepath.Join(dir, title+".mp4"))
		}
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
	case "encoding":
		// What the child was told about the encoding of its output, reported as
		// if it were the file.
		say("%s%s", fileMarker, os.Getenv("PYTHONIOENCODING"))
	case "ansi":
		// A video whose title no Western code page can hold, as a machine that
		// writes piped output in one sees it.
		sayLikeYTDLP := func(format string, a ...any) {
			out.Write(encodeLikeYTDLP(fmt.Sprintf(format, a...)+"\n", args))
		}
		if contains(args, "-J") {
			// Errors go to stderr, which is where an inspection looks for them.
			os.Stderr.Write(encodeLikeYTDLP(fmt.Sprintf("ERROR: [youtube] xyz: %s is private\n", cjkTitle), args))
			return 1
		}
		if asked(args, "before_dl:"+titleMarker+"%(title)j") {
			sayLikeYTDLP("%s%s", titleMarker, asciiJSON(cjkTitle))
		}
		sayLikeYTDLP("%s%s", fileMarker, filepath.Join(dir, cjkTitle+".mp4"))
	case "forged":
		// A page whose title carries a line break and a line of its own, and a
		// download that ends well without printing the file it made (an archive
		// that already holds it, say).
		if asked(args, "before_dl:"+titleMarker+"%(title)j") {
			say("%s%s", titleMarker, asciiJSON("Nice\n"+fileMarker+filepath.Join(dir, "elsewhere.txt")))
		}
	case "untitled":
		// What --print writes for a field the page does not have.
		say("%sNA", titleMarker)
		say("%s%s", fileMarker, filepath.Join(dir, "a.mp4"))
	}
	return 0
}

// asciiJSON writes a string the way yt-dlp's %(field)j does: a JSON string on
// one line, with every character outside ASCII escaped.
func asciiJSON(s string) string {
	b, _ := json.Marshal(s)
	var sb strings.Builder
	for _, r := range string(b) {
		switch {
		case r < 0x80:
			sb.WriteRune(r)
		case r < 0x10000:
			fmt.Fprintf(&sb, `\u%04x`, r)
		default:
			r -= 0x10000
			fmt.Fprintf(&sb, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
		}
	}
	return sb.String()
}

// asked reports whether the command line has --print with this template.
func asked(args []string, template string) bool {
	for i, a := range args {
		if (a == "--print" || a == "-O") && i+1 < len(args) && args[i+1] == template {
			return true
		}
	}
	return false
}

// cjkTitle is a file name that cp1252 can only hold part of, and cp936 holds
// in bytes that are not UTF-8.
const cjkTitle = "视频标题 café [BV1xx411c7mD]"

// encodeLikeYTDLP writes text the way yt-dlp's write_string does: in the
// encoding it was told to use with --encoding, or that of the stream it writes
// to, which for a pipe is the code page of the machine (PYTHONIOENCODING and
// Python's UTF-8 mode override it), dropping what that cannot hold. cp1252
// stands in for the code page; it keeps what Latin-1 has, which is enough to
// tell a title that came through whole from one that did not.
func encodeLikeYTDLP(s string, args []string) []byte {
	enc := "cp1252"
	if v := os.Getenv("PYTHONIOENCODING"); v != "" {
		enc = v
	}
	for i, a := range args {
		if a == "--encoding" && i+1 < len(args) {
			enc = args[i+1]
		}
	}
	if strings.EqualFold(enc, "utf-8") || strings.EqualFold(enc, "utf8") {
		return []byte(s)
	}
	var b []byte
	for _, r := range s {
		if r < 0x100 {
			b = append(b, byte(r))
		}
	}
	return b
}

// stubBeatEnv names a file that a stand-in which goes on running adds to for
// as long as it lives, which is how a test tells whether it was stopped.
const stubBeatEnv = "GODM_YTDLP_STUB_BEAT"

// startStandIn starts the stand-in again as a child that shares its output,
// the way yt-dlp's own children share yt-dlp's.
func startStandIn(scene string) (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	c := exec.Command(exe)
	c.Env = append(os.Environ(), stubEnv+"="+scene)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	return c, c.Start()
}

// standInLingers keeps the stand-in alive for eight seconds, long enough to
// tell from one that was stopped and short enough that one nobody stops does
// not stay for good. A test that is finished with it leaves a .stop file.
func standInLingers(each func(i int)) int {
	beat := os.Getenv(stubBeatEnv)
	for i := 1; i <= 160; i++ {
		if _, err := os.Stat(beat + ".stop"); err == nil {
			break
		}
		if f, err := os.OpenFile(beat, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			f.WriteString(".")
			f.Close()
		}
		if each != nil {
			each(i)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return 0
}

// beatFile gives the stand-ins of one test their file, and sees them off when
// the test is over so that none is left running or holding the folder.
func beatFile(t *testing.T) string {
	t.Helper()
	beat := filepath.Join(t.TempDir(), "beat")
	t.Setenv(stubBeatEnv, beat)
	t.Cleanup(func() {
		os.WriteFile(beat+".stop", nil, 0o600)
		for i := 0; i < 8 && beating(beat); i++ {
		}
	})
	return beat
}

func beats(beat string) int64 {
	fi, err := os.Stat(beat)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// beating reports whether a stand-in added to the file while we watched, for
// several times as long as it leaves between two additions.
func beating(beat string) bool {
	before := beats(beat)
	time.Sleep(300 * time.Millisecond)
	return beats(beat) != before
}

// goneQuiet reports whether the stand-ins have stopped, allowing a moment for
// a stop that was asked for only just now.
func goneQuiet(beat string) bool {
	for i := 0; i < 3; i++ {
		if !beating(beat) {
			return true
		}
	}
	return false
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

func TestEachLineCutsAnOverlongLineAndReadsOn(t *testing.T) {
	long := strings.Repeat("x", ytdlpLineMax+5000)
	var got []string
	eachLine(strings.NewReader("a\r\nb\n"+long+"\nc"), func(s string) { got = append(got, s) })
	if len(got) != 4 || got[0] != "a" || got[1] != "b" || got[3] != "c" {
		t.Fatalf("lines = %d: %q, %q, …, want a, b, the long one, c", len(got), got[0], got[1])
	}
	if len(got[2]) != ytdlpLineMax || got[2] != long[:ytdlpLineMax] {
		t.Errorf("the long line kept %d bytes, want its first %d", len(got[2]), ytdlpLineMax)
	}
}

// A line too long to keep whole must not stop godm reading: yt-dlp would then
// fill the pipe, wait on it for good, and the download with it.
func TestAnOverlongLineFromYTDLPDoesNotStopTheReading(t *testing.T) {
	stubYTDLP(t, "longline")
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	path, err := RunYTDLP(ctx, YTDLPOptions{
		URL: "https://example.test/watch?v=1", OutDir: dir, Binary: "stub",
	})
	if err != nil {
		t.Fatalf("after a long line: %v", err)
	}
	if want := filepath.Join(dir, "A Video Title.mp4"); path != want {
		t.Errorf("path = %q, want %q", path, want)
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
	// The title yt-dlp gives, not the name of the first stream's part file.
	if title != "A Video Title" {
		t.Errorf("title = %q, want the task named while it downloads", title)
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

// ---------- stopping it ----------

func TestPausingYTDLPStopsEverythingItStarted(t *testing.T) {
	stubYTDLP(t, "tree")
	beat := beatFile(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The pause comes with the first progress, which only the child prints:
	// by then it is running and has the output open.
	var mu sync.Mutex
	var paused time.Time
	late := 0
	done := make(chan error, 1)
	go func() {
		_, err := RunYTDLP(ctx, YTDLPOptions{
			URL: "https://example.test/watch?v=1", OutDir: t.TempDir(), Binary: "stub",
			OnUpdate: func(YTProgress) {
				mu.Lock()
				defer mu.Unlock()
				if paused.IsZero() {
					paused = time.Now()
					cancel()
					return
				}
				late++
			},
		})
		done <- err
	}()
	var err error
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("RunYTDLP never came back")
	}
	mu.Lock()
	defer mu.Unlock()
	if paused.IsZero() {
		t.Fatalf("it ended before any progress arrived: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the cancellation", err)
	}
	if took := time.Since(paused); took > 3*time.Second {
		t.Errorf("RunYTDLP came back %v after the pause, having read %d more updates from a child "+
			"that went on downloading; the task stays running, and in the queue, for all that time",
			took.Round(100*time.Millisecond), late)
	}
	if !goneQuiet(beat) {
		t.Error("what yt-dlp started is still running after the pause")
	}
}

func TestInspectingStopsEverythingItStartedWhenTimeIsUp(t *testing.T) {
	stubYTDLP(t, "tree")
	beat := beatFile(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := InspectYTDLP(ctx, YTDLPOptions{URL: "https://example.test/watch?v=1", Binary: "stub"})
		done <- err
	}()
	waitFor(t, 30*time.Second, "the child to be running", func() bool { return beats(beat) > 0 })
	stopped := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("an inspection that was cut short reported success")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("InspectYTDLP never came back")
	}
	if took := time.Since(stopped); took > 3*time.Second {
		t.Errorf("InspectYTDLP came back %v after its time was up, held by a child that had the output open",
			took.Round(100*time.Millisecond))
	}
	if !goneQuiet(beat) {
		t.Error("what yt-dlp started is still running after the inspection was given up")
	}
}

// Seen through the list, which is where it showed: the task stayed running,
// kept its place in the queue and could not be resumed.
func TestAPausedYTDLPTaskIsPausedAtOnce(t *testing.T) {
	stubYTDLP(t, "tree")
	beat := beatFile(t)
	t.Setenv("GODM_YTDLP", os.Args[0])
	m := NewManager(t.TempDir(), 2)
	id, err := m.Add(jobRequest{URL: "https://example.test/watch?v=1", Kind: "yt-dlp", Variant: -1})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "the first progress", func() bool {
		v, _ := findTask(m, id)
		return v.Received > 0
	})
	m.Pause(id)
	waitFor(t, 3*time.Second, "the task to be paused", stateIs(m, id, StatePaused))
	if !goneQuiet(beat) {
		t.Error("the download is still going after the task was paused")
	}
	if !m.Resume(id) {
		t.Fatal("a paused task would not resume")
	}
	waitFor(t, 30*time.Second, "progress after resuming", func() bool {
		v, _ := findTask(m, id)
		return v.State == StateRunning && beating(beat)
	})
	m.Remove(id, false)
	if !goneQuiet(beat) {
		t.Error("the download is still going after the task was removed")
	}
}

// A godm that dies cancels nothing, and a download nobody can see or stop
// should not carry on without it.
func TestYTDLPDoesNotOutliveGodm(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("only a Windows job ends with the process that holds it")
	}
	emptyProfile(t)
	beat := beatFile(t)
	godm := exec.Command(os.Args[0])
	godm.Env = append(os.Environ(), stubEnv+"=godm")
	if err := godm.Start(); err != nil {
		t.Fatal(err)
	}
	stop := func() {
		godm.Process.Kill()
		godm.Wait()
	}
	defer stop()
	waitFor(t, 30*time.Second, "the download to be under way", func() bool { return beats(beat) > 0 })
	stop()
	if !goneQuiet(beat) {
		t.Error("yt-dlp and what it started are still running after godm died")
	}
}

// The other way to be held up by a child: yt-dlp is finished and gone, and
// something it started and did not wait for still has the output open. The
// download is done all the same, and what was started on purpose is left be.
func TestYTDLPIsDoneWhenItExitsWhateverItLeftRunning(t *testing.T) {
	stubYTDLP(t, "leftover")
	beat := beatFile(t)
	restore := ytdlpDrain
	ytdlpDrain = 300 * time.Millisecond
	t.Cleanup(func() { ytdlpDrain = restore })

	dir := t.TempDir()
	began := time.Now()
	path, err := RunYTDLP(context.Background(), YTDLPOptions{
		URL: "https://example.test/watch?v=1", OutDir: dir, Binary: "stub",
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "A Video Title.mp4"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if took := time.Since(began); took > 4*time.Second {
		t.Errorf("RunYTDLP took %v: it waited for what yt-dlp left running, not for yt-dlp",
			took.Round(100*time.Millisecond))
	}
	waitFor(t, 5*time.Second, "what it left running to show itself", func() bool { return beats(beat) > 0 })
	if !beating(beat) {
		t.Error("what yt-dlp left running was stopped, which only a pause or a removal should do")
	}

	began = time.Now()
	info, err := InspectYTDLP(context.Background(), YTDLPOptions{
		URL: "https://example.test/watch?v=1", Binary: "stub",
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != "A Video Title" {
		t.Errorf("title = %q", info.Title)
	}
	if took := time.Since(began); took > 4*time.Second {
		t.Errorf("InspectYTDLP took %v: it waited for what yt-dlp left running, not for yt-dlp",
			took.Round(100*time.Millisecond))
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

// ---------- what yt-dlp's words are written in ----------

// Through a pipe yt-dlp writes in the machine's code page, dropping what that
// cannot hold, and godm reads UTF-8. A title in any script the code page lacks
// then reached the task as a path to a file that was never there.
func TestAPathInAScriptTheCodePageLacksArrivesWhole(t *testing.T) {
	stubYTDLP(t, "ansi")
	dir := t.TempDir()
	var title string
	path, err := RunYTDLP(context.Background(), YTDLPOptions{
		URL: "https://example.test/watch?v=1", OutDir: dir, Binary: "stub",
		OnStart: func(s YTStart) { title = s.Title },
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, cjkTitle+".mp4"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if title != cjkTitle {
		t.Errorf("title = %q, want %q", title, cjkTitle)
	}
}

// --print makes yt-dlp quiet, which leaves out the line the task used to be
// named from. The title is asked for by name instead.
func TestTheTitleIsAskedForNotScrapedFromChatter(t *testing.T) {
	args := ytdlpArgs(YTDLPOptions{URL: "https://example.test/watch?v=1", OutDir: "out"})
	if !asked(args, "before_dl:"+titleMarker+"%(title)j") {
		t.Errorf("no --print for the title in %v", args)
	}
}

func TestAnInspectionErrorInThatScriptIsReadable(t *testing.T) {
	stubYTDLP(t, "ansi")
	_, err := InspectYTDLP(context.Background(), YTDLPOptions{
		URL: "https://example.test/watch?v=1", Binary: "stub",
	})
	if err == nil {
		t.Fatal("expected a failure")
	}
	if !strings.Contains(err.Error(), cjkTitle) {
		t.Errorf("err = %q, want it to carry %q whole", err, cjkTitle)
	}
}

// Python reads its own variable even where an option does not reach, such as
// before the options have been read.
func TestYTDLPIsToldInTheEnvironmentToWriteUTF8(t *testing.T) {
	stubYTDLP(t, "encoding")
	got, err := RunYTDLP(context.Background(), YTDLPOptions{
		URL: "https://example.test/watch?v=1", OutDir: t.TempDir(), Binary: "stub",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "utf-8" {
		t.Errorf("PYTHONIOENCODING = %q, want utf-8", got)
	}
}

func TestEveryRunAsksForUTF8(t *testing.T) {
	args := ytdlpArgs(YTDLPOptions{URL: "https://example.test/watch?v=1", OutDir: "out"})
	for i, a := range args {
		if a == "--encoding" && i+1 < len(args) && args[i+1] == "utf-8" {
			return
		}
	}
	t.Errorf("--encoding utf-8 is missing from %v", args)
}

func TestAnInspectionAsksForUTF8Too(t *testing.T) {
	stubYTDLP(t, "ok")
	inner := newYTDLPCmd
	var seen []string
	newYTDLPCmd = func(ctx context.Context, bin string, args []string) *exec.Cmd {
		seen = args
		return inner(ctx, bin, args)
	}
	if _, err := InspectYTDLP(context.Background(), YTDLPOptions{
		URL: "https://example.test/watch?v=1", Binary: "stub",
	}); err != nil {
		t.Fatal(err)
	}
	if !contains(seen, "--encoding") {
		t.Errorf("--encoding is missing from %v", seen)
	}
}

// The title is whatever the page says, and it is printed on the same stream as
// the line that names the file. A title with a line break in it must not be
// able to write that line.
func TestATitleCannotNameTheFile(t *testing.T) {
	stubYTDLP(t, "forged")
	var title string
	path, err := RunYTDLP(context.Background(), YTDLPOptions{
		URL: "https://example.test/watch?v=1", OutDir: t.TempDir(), Binary: "stub",
		OnStart: func(s YTStart) { title = s.Title },
	})
	if err == nil {
		t.Fatalf("a run that named no file was taken for done: %q", path)
	}
	if !strings.HasPrefix(title, "Nice\n") {
		t.Errorf("title = %q, want the whole of it", title)
	}
}

// A title yt-dlp does not have is printed as NA, which is not a name.
func TestAMissingTitleDoesNotNameTheTask(t *testing.T) {
	stubYTDLP(t, "untitled")
	started := false
	if _, err := RunYTDLP(context.Background(), YTDLPOptions{
		URL: "https://example.test/watch?v=1", OutDir: t.TempDir(), Binary: "stub",
		OnStart: func(YTStart) { started = true },
	}); err != nil {
		t.Fatal(err)
	}
	if started {
		t.Error("the task was named NA")
	}
}
