package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// useSystemPath makes the system's PATH, as godm reads it again and again,
// these folders for the length of a test.
func useSystemPath(t *testing.T, dirs ...string) {
	t.Helper()
	was := currentSystemPath
	currentSystemPath = func() []string { return dirs }
	t.Cleanup(func() { currentSystemPath = was })
}

// fakeTool makes a file that counts as a program by name: an .exe on Windows,
// where that is what makes it one, and anything marked executable elsewhere.
func fakeTool(t *testing.T, dir, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("not really a program"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func envValue(env []string, name string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && sameEnvName(k, name) {
			return v, true
		}
	}
	return "", false
}

func TestExpandPercent(t *testing.T) {
	vars := map[string]string{"SystemRoot": `C:\WINDOWS`, "USERPROFILE": `C:\Users\me`, "EMPTY": ""}
	get := func(n string) string { return vars[n] }
	for in, want := range map[string]string{
		`%SystemRoot%\system32`:           `C:\WINDOWS\system32`,
		`%USERPROFILE%\bin;%SystemRoot%`:  `C:\Users\me\bin;C:\WINDOWS`,
		`C:\Tools`:                        `C:\Tools`,
		`%NOSUCH%\x`:                      `%NOSUCH%\x`,
		`%EMPTY%\x`:                       `%EMPTY%\x`,
		`100%\x`:                          `100%\x`,
		`%%`:                              `%%`,
		`%NOSUCH%USERPROFILE%`:            `%NOSUCH%USERPROFILE%`,
		`%NOSUCH%%SystemRoot%\x`:          `%NOSUCH%C:\WINDOWS\x`,
		`%SystemRoot%%USERPROFILE%`:       `C:\WINDOWSC:\Users\me`,
		`C:\a%b;%SystemRoot%\c`:           `C:\a%b;%SystemRoot%\c`,
		``:                                ``,
		`%SystemRoot%\%USERPROFILE%\more`: `C:\WINDOWS\C:\Users\me\more`,
	} {
		if got := expandPercent(in, get); got != want {
			t.Errorf("expandPercent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanPathDirs(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	got := cleanPathDirs([]string{"", "  ", `"` + a + `"`, "relative" + string(filepath.Separator) + "dir", a, " " + b + " ", strings.ToUpper(a)})
	want := []string{a, b}
	if runtime.GOOS != "windows" {
		// Another spelling of a folder is another folder where names are case-sensitive.
		want = append(want, strings.ToUpper(a))
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("cleaned to %q, want %q: no blanks, no quotes, nothing that is not a full path, no folder twice", got, want)
	}
}

func TestSetEnvReplacesWhateverSpellingIsThere(t *testing.T) {
	env := []string{"A=1", "Temp=old", "TEMP=older", "B=2"}
	got := setEnv(env, "TEMP", "new")
	if runtime.GOOS == "windows" {
		if len(got) != 3 || got[len(got)-1] != "TEMP=new" || strings.Contains(strings.Join(got, ";"), "old") {
			t.Errorf("got %q: both spellings are the one variable and are to be replaced", got)
		}
	} else if v, _ := envValue(got, "TEMP"); v != "new" || len(got) != 4 {
		t.Errorf("got %q", got)
	}
	if env[1] != "Temp=old" {
		t.Error("the list that was given was changed")
	}
}

func TestPathDirsAreAddedAfterTheOnesAlreadyThere(t *testing.T) {
	own1, own2, new1, new2 := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	sep := string(os.PathListSeparator)
	env := []string{"A=1", "Path=" + own1 + sep + own2, "B=2"}

	got := withPathDirs(env, []string{own2, new1, strings.ToUpper(new1), new2, own1})
	v, ok := envValue(got, "Path")
	if !ok {
		t.Fatalf("no PATH in %q", got)
	}
	// The same folder spelled in capitals is the same folder on Windows only.
	want := strings.Join([]string{own1, own2, new1, new2}, sep)
	if runtime.GOOS != "windows" {
		want = strings.Join([]string{own1, own2, new1, strings.ToUpper(new1), new2}, sep)
	}
	if v != want {
		t.Errorf("PATH = %q, want %q: its own folders first, then the new ones, none twice", v, want)
	}
	if runtime.GOOS == "windows" && got[1][:5] != "Path=" {
		t.Errorf("the variable was renamed: %q", got[1])
	}
	if len(got) != len(env) || got[0] != "A=1" || got[2] != "B=2" {
		t.Errorf("the rest of the environment was changed: %q", got)
	}
	if env[1] != "Path="+own1+sep+own2 {
		t.Error("the list that was given was changed")
	}

	// Nothing new: the very same list comes back.
	if again := withPathDirs(env, []string{own1, own2}); &again[0] != &env[0] {
		t.Error("a PATH that already holds everything was copied")
	}
	// No PATH at all: there is one now.
	if v, _ := envValue(withPathDirs([]string{"A=1"}, []string{new1}), "PATH"); v != new1 {
		t.Errorf("PATH = %q, want %q", v, new1)
	}
	// An empty one, or one that ends in a separator, makes no empty folder.
	if v, _ := envValue(withPathDirs([]string{"PATH="}, []string{new1}), "PATH"); v != new1 {
		t.Errorf("PATH = %q, want %q", v, new1)
	}
	if v, _ := envValue(withPathDirs([]string{"PATH=" + own1 + sep}, []string{new1}), "PATH"); v != own1+sep+new1 {
		t.Errorf("PATH = %q, want %q", v, own1+sep+new1)
	}
}

// godm is started once, by whatever starts the browser, and keeps the PATH it
// had then. A tool installed since is on the system's PATH.
func TestYTDLPInstalledAfterGodmStartedIsFound(t *testing.T) {
	t.Setenv("GODM_YTDLP", "")
	t.Setenv("PATH", t.TempDir()) // godm's own: nothing in it
	installed := t.TempDir()
	reads := 0
	was := currentSystemPath
	currentSystemPath = func() []string { reads++; return []string{installed} }
	t.Cleanup(func() { currentSystemPath = was })

	if p, err := findYTDLP(); err == nil {
		t.Fatalf("found %s before anything was installed", p)
	}
	if YTDLPAvailable() {
		t.Error("yt-dlp is offered with none installed")
	}

	want := fakeTool(t, installed, "yt-dlp")
	got, err := findYTDLP()
	if err != nil {
		t.Fatalf("yt-dlp is on the system's PATH and was not found: %v", err)
	}
	if got != want {
		t.Errorf("found %s, want %s", got, want)
	}
	if !YTDLPAvailable() {
		t.Error("yt-dlp is not offered though it is installed")
	}
	if reads < 3 {
		t.Errorf("the system's PATH was read %d times for three questions; it is to be read each time", reads)
	}
}

func TestGodmsOwnPathStillComesFirst(t *testing.T) {
	t.Setenv("GODM_YTDLP", "")
	own, system := t.TempDir(), t.TempDir()
	t.Setenv("PATH", own)
	useSystemPath(t, system)
	fakeTool(t, system, "yt-dlp")
	want := fakeTool(t, own, "yt-dlp")
	if got, err := findYTDLP(); err != nil || got != want {
		t.Errorf("found %q (%v), want the one on godm's own PATH, %s", got, err, want)
	}
}

// Where the registry cannot be read, as on macOS and Linux, nothing changes.
func TestNoSystemPathLeavesYTDLPMissing(t *testing.T) {
	t.Setenv("GODM_YTDLP", "")
	t.Setenv("PATH", t.TempDir())
	useSystemPath(t) // none
	_, err := findYTDLP()
	var missing *YTDLPMissingError
	if !errors.As(err, &missing) {
		t.Errorf("err = %v, want the advice for installing it", err)
	}
}

func TestAPlayerInstalledAfterGodmStartedIsFoundToo(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	installed := t.TempDir()
	useSystemPath(t, installed)
	want := fakeTool(t, installed, "mpv")
	exe, isMPV := findPlayer()
	if exe != want || !isMPV {
		t.Errorf("findPlayer() = %q, %v, want %s, true", exe, isMPV, want)
	}
}

// What yt-dlp needs, Deno for YouTube and ffmpeg for merging, comes as other
// packages in other folders, and yt-dlp finds them on its PATH: the one it
// is given has to be the system's as well as godm's.
func TestYTDLPIsStartedWithThePathTheSystemHasNow(t *testing.T) {
	stubYTDLP(t, "path")
	own, deno, ffmpeg := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("PATH", own)
	useSystemPath(t, own, deno, ffmpeg)

	got, err := RunYTDLP(context.Background(), YTDLPOptions{
		URL: "https://example.test/watch?v=1", OutDir: t.TempDir(), Binary: "stub",
	})
	if err != nil {
		t.Fatal(err)
	}
	sep := string(os.PathListSeparator)
	if want := strings.Join([]string{own, deno, ffmpeg}, sep); got != want {
		t.Errorf("yt-dlp was started with PATH %q, want %q", got, want)
	}
}

// An inspection runs yt-dlp too, and needs Deno just as much.
func TestAnInspectionGetsTheSystemsPathToo(t *testing.T) {
	stubYTDLP(t, "ok")
	deno := t.TempDir()
	useSystemPath(t, deno)
	inner := newYTDLPCmd
	var seen *exec.Cmd
	newYTDLPCmd = func(ctx context.Context, bin string, args []string) *exec.Cmd {
		seen = inner(ctx, bin, args)
		return seen
	}
	if _, err := InspectYTDLP(context.Background(), YTDLPOptions{
		URL: "https://example.test/watch?v=1", Binary: "stub",
	}); err != nil {
		t.Fatal(err)
	}
	v, _ := envValue(seen.Env, "PATH")
	if !strings.Contains(v, deno) {
		t.Errorf("yt-dlp was inspected with PATH %q, which lacks %s", v, deno)
	}
}
