package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// None of these tests may reach GitHub, run winget, or take the winget or the
// Deno of the computer they run on for their own. untouchable puts something in
// the place of each that fails loudly, and a test that wants one replaces it
// for its own length.
func untouchable() {
	fetchLatestYTDLP = func(ctx context.Context) (string, error) {
		return "", errors.New("the tests do not reach GitHub")
	}
	newWingetCmd = func(ctx context.Context, bin string, args []string) *exec.Cmd {
		c := exec.CommandContext(ctx, os.Args[0])
		c.Env = append(os.Environ(), stubEnv+"=refuse")
		return c
	}
	findWinget = func() string { return "" }
	lookupTool = func(string) (string, error) { return "", exec.ErrNotFound }
}

// realFetchLatest is the function that asks GitHub, kept from before untouchable
// replaced it, for the test that points it at a server of its own.
var realFetchLatest = fetchLatestYTDLP

// replace puts v in *p until the test is over.
func replace[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// ---------- the stand-ins ----------

// What the stand-ins of yt-dlp and winget are told through the environment.
const (
	stubVersionFile = "GODM_STUB_VERSION_FILE" // the version the yt-dlp stand-in says it is
	stubArgsFile    = "GODM_STUB_ARGS_FILE"    // where each is to write down what it was asked
	stubUpdateTo    = "GODM_STUB_UPDATE_TO"    // what yt-dlp -U makes the version; empty fails it
	stubWinget      = "GODM_STUB_WINGET"       // what winget does for each package id, as JSON
	stubVersionWait = "GODM_STUB_VERSION_WAIT" // milliseconds yt-dlp takes to say its version
	stubHang        = "GODM_STUB_HANG"         // "version" or "update": yt-dlp never gets to the end of it
)

// wingetStep is what the winget stand-in does for one package.
type wingetStep struct {
	Exit int64  `json:"exit"`
	To   string `json:"to"`   // the version yt-dlp is after a run that works
	Out  string `json:"out"`  // what it prints
	Hang bool   `json:"hang"` // it never ends
}

// hangs is a program that does not end until it is killed. (A bare select
// would be taken by the runtime for a deadlock and end the program at once.)
func hangs() int {
	time.Sleep(time.Hour)
	return 0
}

// playUpdater is the test binary acting as yt-dlp (asked for its version, or to
// update itself), as winget, or as a winget that no test may run.
func playUpdater(scene string, args []string) int {
	if f := os.Getenv(stubArgsFile); f != "" {
		if w, err := os.OpenFile(f, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			fmt.Fprintf(w, "%s %s\n", scene, strings.Join(args, " "))
			w.Close()
		}
	}
	versionFile := os.Getenv(stubVersionFile)
	switch scene {
	case "refuse":
		fmt.Println("a test ran winget")
		return 99
	case "ytool":
		switch {
		case contains(args, "--version"):
			if ms, _ := strconv.Atoi(os.Getenv(stubVersionWait)); ms > 0 {
				time.Sleep(time.Duration(ms) * time.Millisecond)
			}
			if os.Getenv(stubHang) == "version" {
				return hangs()
			}
			b, err := os.ReadFile(versionFile)
			if err != nil {
				fmt.Fprintln(os.Stderr, "ERROR: no version")
				return 1
			}
			if strings.TrimSpace(string(b)) == "crash" {
				// A broken installation: a complaint that is not prefixed as one.
				fmt.Fprintln(os.Stderr, "ModuleNotFoundError: No module named 'yt_dlp'")
				return 1
			}
			// A warning on the way, as an old yt-dlp writes one.
			fmt.Fprintln(os.Stderr, "WARNING: this version is older than 90 days")
			fmt.Println(strings.TrimSpace(string(b)))
			return 0
		case contains(args, "-U"):
			if os.Getenv(stubHang) == "update" {
				return hangs()
			}
			to := os.Getenv(stubUpdateTo)
			if to == "" {
				fmt.Println("ERROR: Unable to write to yt-dlp.exe.new; Permission denied")
				return 100
			}
			os.WriteFile(versionFile, []byte(to+"\n"), 0o600)
			fmt.Printf("Current version: stable@2026.06.01 from yt-dlp/yt-dlp\nUpdating to stable@%s from yt-dlp/yt-dlp ...\n", to)
			return 0
		}
	case "winget":
		id := ""
		for i, a := range args {
			if a == "--id" && i+1 < len(args) {
				id = args[i+1]
			}
		}
		var plan map[string]wingetStep
		json.Unmarshal([]byte(os.Getenv(stubWinget)), &plan)
		step, ok := plan[id]
		if !ok {
			fmt.Println("No installed package found matching input criteria.")
			return 1
		}
		fmt.Print(step.Out)
		if step.Hang {
			return hangs()
		}
		if step.Exit == 0 && step.To != "" {
			os.WriteFile(versionFile, []byte(step.To+"\n"), 0o600)
		}
		return int(step.Exit)
	}
	return 0
}

// updateRig is a computer with a yt-dlp on it, of the kind a test asks for, that
// says the version it is told to, and a winget and a Deno if asked for.
type updateRig struct {
	t           *testing.T
	root        string
	bin         string
	versionFile string
	argsFile    string
}

// newUpdateRig puts a file where a yt-dlp of the kind is kept, points godm at
// it, and stands in for yt-dlp in every run that asks for its version or an
// update. Anything else yt-dlp is asked is played as the scene says, if one is
// given. It also gives the test a gate of its own.
func newUpdateRig(t *testing.T, kind, version, scene string) *updateRig {
	t.Helper()
	r := &updateRig{t: t, root: t.TempDir()}
	dir, size := filepath.Join(r.root, "tools"), int64(ytStandaloneMin+1)
	switch kind {
	case ytInstWinget:
		dir = filepath.Join(r.root, "AppData", "Local", "Microsoft", "WinGet", "Packages",
			"yt-dlp.yt-dlp_Microsoft.Winget.Source_8wekyb3d8bbwe")
	case ytInstPip:
		dir, size = filepath.Join(r.root, "Python313", "Scripts"), 100
	case ytInstScoop:
		dir = filepath.Join(r.root, "scoop", "apps", "yt-dlp", "current")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r.bin = filepath.Join(dir, "yt-dlp.exe")
	f, err := os.Create(r.bin)
	if err != nil {
		t.Fatal(err)
	}
	f.Truncate(size)
	f.Close()
	r.versionFile = filepath.Join(r.root, "version")
	r.argsFile = filepath.Join(r.root, "args")
	if version != "" {
		os.WriteFile(r.versionFile, []byte(version+"\n"), 0o600)
	}
	emptyProfile(t)
	t.Setenv("GODM_YTDLP", r.bin)
	t.Setenv(stubVersionFile, r.versionFile)
	t.Setenv(stubArgsFile, r.argsFile)
	replace(t, &newYTDLPCmd, func(ctx context.Context, bin string, args []string) *exec.Cmd {
		s := "ytool"
		if scene != "" && !contains(args, "--version") && !contains(args, "-U") {
			s = scene
		}
		c := exec.CommandContext(ctx, os.Args[0], args...)
		c.Env = append(os.Environ(), stubEnv+"="+s)
		return c
	})
	replace(t, &ytdlpGate, &ytGate{})
	return r
}

// withWinget gives the computer a winget that does what the plan says for each
// package it is asked to upgrade.
func (r *updateRig) withWinget(plan map[string]wingetStep) {
	r.t.Helper()
	b, _ := json.Marshal(plan)
	r.t.Setenv(stubWinget, string(b))
	replace(r.t, &findWinget, func() string { return "winget-stand-in" })
	replace(r.t, &newWingetCmd, func(ctx context.Context, bin string, args []string) *exec.Cmd {
		c := exec.CommandContext(ctx, os.Args[0], args...)
		c.Env = append(os.Environ(), stubEnv+"=winget")
		return c
	})
}

// withDeno puts a Deno, in the folder winget keeps it in, on the computer.
func (r *updateRig) withDeno() {
	r.t.Helper()
	deno := filepath.Join(r.root, "AppData", "Local", "Microsoft", "WinGet", "Packages",
		"DenoLand.Deno_Microsoft.Winget.Source_8wekyb3d8bbwe", "deno.exe")
	replace(r.t, &lookupTool, func(name string) (string, error) {
		if name == "deno" {
			return deno, nil
		}
		return "", exec.ErrNotFound
	})
}

// calls lists what the stand-ins were asked, one run to a line, as "scene args".
func (r *updateRig) calls(scene string) []string {
	b, _ := os.ReadFile(r.argsFile)
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.HasPrefix(line, scene+" ") {
			out = append(out, strings.TrimPrefix(line, scene+" "))
		}
	}
	return out
}

func (r *updateRig) version() string {
	b, _ := os.ReadFile(r.versionFile)
	return strings.TrimSpace(string(b))
}

// updateAndWait starts an update and waits for it to be over.
func updateAndWait(t *testing.T, tool *ytTool) ytStatus {
	t.Helper()
	if err := tool.startUpdate(); err != nil {
		t.Fatalf("the update was refused: %v", err)
	}
	waitFor(t, 20*time.Second, "the update to end", func() bool { return !tool.status().Updating })
	st := tool.status()
	if st.Result == nil {
		t.Fatal("the update ended without a result")
	}
	return st
}

const wingetYTDLPCall = "upgrade --id yt-dlp.yt-dlp --exact --source winget --scope user --silent " +
	"--accept-package-agreements --accept-source-agreements --disable-interactivity"
const wingetDenoCall = "upgrade --id DenoLand.Deno --exact --source winget --scope user --silent " +
	"--accept-package-agreements --accept-source-agreements --disable-interactivity"

// ---------- versions ----------

func TestVersionsAreReadAsYTDLPWritesThem(t *testing.T) {
	for in, want := range map[string]string{
		"2026.08.19":                "2026.8.19",
		" 2026.08.19\r\n":           "2026.8.19",
		"2022.06.22.1":              "2022.6.22.1", // made again the same day
		"2026.10.15.232837":         "2026.10.15.232837",
		"nightly@2026.10.15.232837": "2026.10.15.232837", // as yt-dlp -v writes it
		"stable@2026.08.19":         "2026.8.19",
	} {
		v, ok := parseYTVersion(in)
		if !ok {
			t.Errorf("%q is a version and was not read as one", in)
			continue
		}
		var parts []string
		for _, n := range v {
			parts = append(parts, strconv.Itoa(n))
		}
		if got := strings.Join(parts, "."); got != want {
			t.Errorf("%q was read as %s, want %s", in, got, want)
		}
	}
	for _, in := range []string{
		"", "dev", "unknown", "1.2.3", "2026.8.19", "2026.13.01", "2026.00.10", "2026.08.32",
		"2026.08.19-dev", "2026.08.19.", "2026.08.19.x", "2026.08.19.12345678901", "v2026.08.19",
		"2026.08.19 (dev)", "a@b",
	} {
		if v, ok := parseYTVersion(in); ok {
			t.Errorf("%q was read as the version %v, and is none", in, v)
		}
	}
}

func TestVersionsAreComparedNumberByNumber(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"2026.08.19", "2026.08.19", 0},
		{"2026.08.19", "2026.08.20", -1},
		{"2026.09.01", "2026.08.31", 1},
		{"2025.12.31", "2026.01.01", -1},
		{"2026.08.19", "2026.08.19.0", 0},
		// Made again the same day: later.
		{"2022.06.22", "2022.06.22.1", -1},
		{"2022.06.22.2", "2022.06.22.10", -1}, // as numbers, not as text
		// A nightly build is dated by its day and the time it was made.
		{"2026.08.19.123456", "2026.08.19", 1},
		{"2026.08.19.123456", "2026.08.20", -1},
		{"2026.08.19.123456", "2026.08.19.234500", -1},
	} {
		a, _ := parseYTVersion(c.a)
		b, _ := parseYTVersion(c.b)
		if got := a.compare(b); got != c.want {
			t.Errorf("%s compared with %s = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := b.compare(a); got != -c.want {
			t.Errorf("%s compared with %s = %d, want %d", c.b, c.a, got, -c.want)
		}
	}
}

// A copy is called old only when both versions are ones that can be compared.
func TestACopyIsCalledOldOnlyBetweenTwoVersions(t *testing.T) {
	for _, c := range []struct {
		have, newest    string
		outdated, known bool
	}{
		{"2026.06.01", "2026.08.19", true, true},
		{"2026.08.19", "2026.08.19", false, true},
		{"2026.10.01", "2026.08.19", false, true}, // ahead of the stable release: a nightly
		{"2026.08.19.123456", "2026.08.19", false, true},
		{"dev", "2026.08.19", false, false},
		{"2026.08.19", "", false, false},
		{"", "", false, false},
		{"custom build", "not a version", false, false},
	} {
		outdated, known := ytOutdated(c.have, c.newest)
		if outdated != c.outdated || known != c.known {
			t.Errorf("have %q, newest %q: outdated %v, known %v; want %v, %v",
				c.have, c.newest, outdated, known, c.outdated, c.known)
		}
	}
}

// ---------- the newest release ----------

func githubReturning(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	replace(t, &ytdlpLatestURL, srv.URL+"/repos/yt-dlp/yt-dlp/releases/latest")
	replace(t, &fetchLatestYTDLP, realFetchLatest)
}

func TestTheNewestReleaseIsTheTagGitHubNames(t *testing.T) {
	var seen *http.Request
	githubReturning(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r
		// The real answer is a long document; the tag is one field of it.
		fmt.Fprint(w, `{"url":"x","tag_name":"2026.09.15","name":"yt-dlp 2026.09.15","prerelease":false,`+
			`"assets":[{"name":"yt-dlp.exe","size":20000000}],"body":"### Changelog"}`)
	})
	tag, err := fetchLatestYTDLP(context.Background())
	if err != nil || tag != "2026.09.15" {
		t.Fatalf("fetch = %q, %v; want the tag", tag, err)
	}
	if seen.URL.Path != "/repos/yt-dlp/yt-dlp/releases/latest" {
		t.Errorf("asked for %s", seen.URL.Path)
	}
	if !strings.Contains(seen.Header.Get("Accept"), "application/vnd.github+json") {
		t.Errorf("Accept = %q", seen.Header.Get("Accept"))
	}
	// GitHub turns away a request that does not say who sends it.
	if !strings.HasPrefix(seen.Header.Get("User-Agent"), "godm/") {
		t.Errorf("User-Agent = %q", seen.Header.Get("User-Agent"))
	}
}

// A failure to find out is a failure, with its reason: never a tag, and so
// never "up to date".
func TestAFailureToLearnTheNewestReleaseIsSaidSo(t *testing.T) {
	reset := time.Now().Add(7 * time.Minute).Unix()
	for name, c := range map[string]struct {
		serve func(w http.ResponseWriter, r *http.Request)
		want  string
	}{
		"a server error": {func(w http.ResponseWriter, r *http.Request) { http.Error(w, "oops", 502) }, "502"},
		"not found":      {func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }, "404"},
		"the rate limit": {func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
			http.Error(w, "rate limit exceeded", 403)
		}, "limit"},
		"a refusal that is not the limit": {func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 403) }, "403"},
		"not json":                        {func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "<html>") }, "could not be read"},
		"no tag":                          {func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"name":"x"}`) }, "not a yt-dlp version"},
		"a tag that is no version":        {func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"tag_name":"nightly"}`) }, `"nightly"`},
	} {
		t.Run(name, func(t *testing.T) {
			githubReturning(t, c.serve)
			tag, err := fetchLatestYTDLP(context.Background())
			if err == nil || tag != "" {
				t.Fatalf("fetch = %q, %v; want an error and no tag", tag, err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the reason is %q, want it to mention %q", err, c.want)
			}
		})
	}
	t.Run("the rate limit says when to try again", func(t *testing.T) {
		githubReturning(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
			http.Error(w, "rate limit exceeded", 403)
		})
		_, err := fetchLatestYTDLP(context.Background())
		if err == nil || !strings.Contains(err.Error(), "7 minutes") {
			t.Errorf("the reason is %v, want the minutes to wait", err)
		}
	})
	t.Run("a server that does not answer", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		githubReturning(t, func(w http.ResponseWriter, r *http.Request) { <-release })
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		_, err := fetchLatestYTDLP(ctx)
		if err == nil || !strings.Contains(err.Error(), "in time") {
			t.Errorf("the reason is %v, want it to say GitHub did not answer in time", err)
		}
	})
	t.Run("no way to the server", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		replace(t, &ytdlpLatestURL, url)
		replace(t, &fetchLatestYTDLP, realFetchLatest)
		_, err := fetchLatestYTDLP(context.Background())
		if err == nil || !strings.Contains(err.Error(), "could not reach GitHub") {
			t.Errorf("the reason is %v", err)
		}
	})
}

// fakeGitHub makes the lookup answer from a list and count the questions.
type fakeGitHub struct {
	calls atomic.Int32
	mu    sync.Mutex
	tag   string
	err   error
	hold  chan struct{} // when set, an answer waits for it to close
}

func (g *fakeGitHub) set(tag string, err error) {
	g.mu.Lock()
	g.tag, g.err = tag, err
	g.mu.Unlock()
}

func (g *fakeGitHub) install(t *testing.T) {
	replace(t, &fetchLatestYTDLP, func(ctx context.Context) (string, error) {
		g.calls.Add(1)
		if g.hold != nil {
			select {
			case <-g.hold:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.tag, g.err
	})
}

func TestTheNewestReleaseIsRememberedForAWhile(t *testing.T) {
	var gh fakeGitHub
	gh.set("2026.08.19", nil)
	gh.install(t)
	var tool ytTool
	ctx := context.Background()

	for range 3 {
		if tag, err := tool.newest(ctx, false); tag != "2026.08.19" || err != nil {
			t.Fatalf("newest = %q, %v", tag, err)
		}
	}
	if n := gh.calls.Load(); n != 1 {
		t.Errorf("GitHub was asked %d times for three looks within the hour, want 1", n)
	}
	gh.set("2026.09.15", nil)
	if tag, _ := tool.newest(ctx, true); tag != "2026.09.15" || gh.calls.Load() != 2 {
		t.Errorf("a look that is forced gave %q after %d questions, want a new answer", tag, gh.calls.Load())
	}

	// Once it is old enough it is asked for again.
	replace(t, &ytLatestTTL, 20*time.Millisecond)
	time.Sleep(60 * time.Millisecond)
	gh.set("2026.09.20", nil)
	if tag, _ := tool.newest(ctx, false); tag != "2026.09.20" {
		t.Errorf("an old answer was kept: %q", tag)
	}
}

func TestAFailureLeavesTheLastAnswerAndIsNotRepeatedAtOnce(t *testing.T) {
	var gh fakeGitHub
	gh.set("2026.08.19", nil)
	gh.install(t)
	var tool ytTool
	ctx := context.Background()
	replace(t, &ytLatestTTL, 20*time.Millisecond)
	if _, err := tool.newest(ctx, false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)

	gh.set("", errors.New("could not reach GitHub"))
	before := gh.calls.Load()
	tag, err := tool.newest(ctx, false)
	if err == nil || tag != "2026.08.19" {
		t.Fatalf("newest = %q, %v; want the earlier answer and the error", tag, err)
	}
	st := tool.status()
	if st.Latest != "2026.08.19" || st.LatestError == "" {
		t.Errorf("status = latest %q, error %q: want both, so that the page cannot call it up to date", st.Latest, st.LatestError)
	}
	// And a computer with no way to GitHub does not ask at every failed download.
	for range 3 {
		tool.newest(ctx, false)
	}
	if n := gh.calls.Load() - before; n != 1 {
		t.Errorf("GitHub was asked %d times in a row after a failure, want 1", n)
	}
	// After a while, or when asked to, it is asked again, and a good answer clears the error.
	gh.set("2026.09.01", nil)
	if tag, err := tool.newest(ctx, true); tag != "2026.09.01" || err != nil {
		t.Errorf("forced look = %q, %v", tag, err)
	}
	if st := tool.status(); st.LatestError != "" {
		t.Errorf("the error stayed after an answer: %q", st.LatestError)
	}
}

func TestLooksThatOverlapShareOneQuestion(t *testing.T) {
	var gh fakeGitHub
	gh.set("2026.08.19", nil)
	gh.hold = make(chan struct{})
	gh.install(t)
	var tool ytTool

	var wg sync.WaitGroup
	tags := make([]string, 5)
	for i := range tags {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tags[i], _ = tool.newest(context.Background(), false)
		}()
	}
	waitFor(t, 5*time.Second, "the first question", func() bool { return gh.calls.Load() == 1 })
	time.Sleep(100 * time.Millisecond) // for the others to join it
	close(gh.hold)
	wg.Wait()
	if n := gh.calls.Load(); n != 1 {
		t.Errorf("GitHub was asked %d times by five looks at once, want 1", n)
	}
	for i, tag := range tags {
		if tag != "2026.08.19" {
			t.Errorf("look %d got %q", i, tag)
		}
	}
}

// A caller that gives up must not take the others' answer with it, nor leave
// the failure in the memory as if GitHub had refused.
func TestAGivenUpLookSaysNothingAboutGitHub(t *testing.T) {
	var gh fakeGitHub
	gh.set("2026.08.19", nil)
	gh.hold = make(chan struct{})
	gh.install(t)
	var tool ytTool

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := tool.newest(ctx, false)
		done <- err
	}()
	waitFor(t, 5*time.Second, "the question", func() bool { return gh.calls.Load() == 1 })
	cancel()
	if err := <-done; err == nil {
		t.Fatal("a look that was given up on returned no error")
	}
	if st := tool.status(); st.LatestError != "" {
		t.Errorf("giving up was remembered as a failure of GitHub's: %q", st.LatestError)
	}
	close(gh.hold)
	if tag, err := tool.newest(context.Background(), false); tag != "2026.08.19" || err != nil {
		t.Errorf("the next look = %q, %v", tag, err)
	}
}

// ---------- how it was installed ----------

func TestHowYTDLPWasInstalledIsToldByWhereAndWhatItIs(t *testing.T) {
	small := filepath.Join(t.TempDir(), "yt-dlp.exe")
	os.WriteFile(small, []byte("MZ launcher"), 0o600)
	big := filepath.Join(t.TempDir(), "yt-dlp.exe")
	f, _ := os.Create(big)
	f.Truncate(ytStandaloneMin + 1)
	f.Close()

	for _, c := range []struct {
		path string
		want string
		cmd  string
	}{
		{`C:\Users\u\AppData\Local\Microsoft\WinGet\Packages\yt-dlp.yt-dlp_Microsoft.Winget.Source_8wekyb3d8bbwe\yt-dlp.exe`, ytInstWinget, ""},
		{`c:\users\u\appdata\local\microsoft\winget\packages\YT-DLP.YT-DLP_Microsoft.Winget.Source_8wekyb3d8bbwe\yt-dlp.exe`, ytInstWinget, ""},
		{`C:\Program Files\WinGet\Packages\yt-dlp.yt-dlp_Microsoft.Winget.Source_8wekyb3d8bbwe\yt-dlp.exe`, ytInstWinget, ""},
		{`C:\Users\u\scoop\apps\yt-dlp\current\yt-dlp.exe`, ytInstScoop, "scoop update yt-dlp"},
		{`C:\Users\u\scoop\shims\yt-dlp.exe`, ytInstScoop, "scoop update yt-dlp"},
		{`C:\ProgramData\chocolatey\bin\yt-dlp.exe`, ytInstChoco, "choco upgrade yt-dlp"},
		{`C:\ProgramData\chocolatey\lib\yt-dlp\tools\yt-dlp.exe`, ytInstChoco, "choco upgrade yt-dlp"},
		{`/opt/homebrew/bin/yt-dlp`, ytInstBrew, "brew upgrade yt-dlp"},
		{`/home/u/.linuxbrew/Cellar/yt-dlp/2026.08.19/bin/yt-dlp`, ytInstBrew, "brew upgrade yt-dlp"},
		{`/home/u/.local/pipx/venvs/yt-dlp/bin/yt-dlp`, ytInstPipx, "pipx upgrade yt-dlp"},
		{`C:\Python313\Lib\site-packages\yt_dlp\__main__.py`, ytInstPip, ytPipCommand},
		// Where it is says nothing: what it is does. Twenty megabytes of Python
		// and yt-dlp is the release; a hundred kilobytes of launcher is pip's.
		{small, ytInstPackage, ytPipCommand},
		{big, ytInstStandalone, ""},
		{filepath.Join(t.TempDir(), "gone.exe"), ytInstPackage, ytPipCommand},
	} {
		got := detectYTInstall(c.path)
		if got.Kind != c.want || got.Command != c.cmd {
			t.Errorf("%s: kind %q, command %q; want %q, %q", c.path, got.Kind, got.Command, c.want, c.cmd)
		}
		if got.godmUpdates() != (c.want == ytInstWinget || c.want == ytInstStandalone) {
			t.Errorf("%s: godm would update it = %v", c.path, got.godmUpdates())
		}
	}
}

// winget keeps its links in one folder and its packages in another; a program
// found through a link is the package's.
func TestAWingetLinkIsTheWingetCopy(t *testing.T) {
	root := t.TempDir()
	pkg := filepath.Join(root, "WinGet", "Packages", "yt-dlp.yt-dlp_Microsoft.Winget.Source_8wekyb3d8bbwe")
	os.MkdirAll(pkg, 0o755)
	target := filepath.Join(pkg, "yt-dlp.exe")
	os.WriteFile(target, []byte("MZ"), 0o600)
	links := filepath.Join(root, "WinGet", "Links")
	os.MkdirAll(links, 0o755)
	link := filepath.Join(links, "yt-dlp.exe")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot make a link here: %v", err)
	}
	if got := detectYTInstall(link).Kind; got != ytInstWinget {
		t.Errorf("the link was taken for a %q install, want winget", got)
	}
}

func TestWingetIsAskedForOnePackageWithoutQuestions(t *testing.T) {
	got := strings.Join(wingetUpgradeArgs("yt-dlp.yt-dlp", "user"), " ")
	if got != wingetYTDLPCall {
		t.Errorf("arguments = %q\nwant %q", got, wingetYTDLPCall)
	}
	got = strings.Join(wingetUpgradeArgs("DenoLand.Deno", ""), " ")
	want := "upgrade --id DenoLand.Deno --exact --source winget --silent --accept-package-agreements " +
		"--accept-source-agreements --disable-interactivity"
	if got != want {
		t.Errorf("arguments = %q\nwant %q", got, want)
	}
}

func TestWingetIsToldTheScopeThePackageWasInstalledIn(t *testing.T) {
	t.Setenv("LOCALAPPDATA", `D:\Profile\Local`)
	for path, want := range map[string]string{
		`C:\Users\u\AppData\Local\Microsoft\WinGet\Packages\yt-dlp.yt-dlp_x\yt-dlp.exe`: "user",
		`D:\Profile\Local\Microsoft\WinGet\Packages\yt-dlp.yt-dlp_x\yt-dlp.exe`:         "user",
		`C:\Program Files\WinGet\Packages\yt-dlp.yt-dlp_x\yt-dlp.exe`:                   "machine",
		`C:\ProgramData\somewhere\yt-dlp.exe`:                                           "machine",
		`E:\tools\yt-dlp.exe`:                                                           "",
	} {
		if got := wingetScope(path); got != want {
			t.Errorf("%s: scope %q, want %q", path, got, want)
		}
	}
}

// fakeExit is a program ending with a code, as winget does.
type fakeExit int

func (e fakeExit) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e fakeExit) ExitCode() int { return int(e) }

func TestWingetsExitCodesAreToldApart(t *testing.T) {
	ten := "  ███████▒▒▒  70%\r  5.30 MB / 7.10 MB\r"
	for _, c := range []struct {
		name         string
		out          string
		err          error
		ok, current  bool
		reasonHas    string
		reasonHasNot string
		code         string // the code the page words the reason by; empty where the wording is winget's
	}{
		{"it worked", ten, nil, true, false, "", "", ""},
		{"nothing newer", "No available upgrade found.\n", fakeExit(0x8A15002B), true, true, "", "", ""},
		{"in use", "", fakeExit(0x8A150101), false, false, "in use", "", "winget-inuse"},
		{"a file in use", "", fakeExit(0x8A150103), false, false, "in use", "", "winget-inuse"},
		{"not winget's", "No installed package found matching input criteria.\n", fakeExit(0x8A150014), false, false, "does not know", "", "winget-nopackage"},
		{"the source agreement", "", fakeExit(0x8A150046), false, false, "agreement", "", "winget-agreement"},
		{"administrator rights needed", "", fakeExit(0x8A150019), false, false, "administrator", "", "winget-admin"},
		{"a user's copy from an administrator's godm", "", fakeExit(0x8A15007D), false, false, "without them", "", "winget-elevated"},
		{"anything else", "Downloading https://x\r" + ten + "Installer hash does not match\n", fakeExit(0x8A150049), false, false,
			"Installer hash does not match (winget exit code 0x8a150049)", "70%", ""},
		{"it was not even started", "", errors.New("fork/exec: access denied"), false, false, "access denied", "", ""},
	} {
		ok, current, why := wingetOutcome(c.out, c.err)
		reason := why.Msg
		if why.Code != c.code {
			t.Errorf("%s: code %q, want %q", c.name, why.Code, c.code)
		}
		if ok != c.ok || current != c.current {
			t.Errorf("%s: ok %v, current %v; want %v, %v", c.name, ok, current, c.ok, c.current)
		}
		if c.reasonHas != "" && !strings.Contains(reason, c.reasonHas) {
			t.Errorf("%s: reason %q, want it to contain %q", c.name, reason, c.reasonHas)
		}
		if c.reasonHasNot != "" && strings.Contains(reason, c.reasonHasNot) {
			t.Errorf("%s: reason %q contains %q", c.name, reason, c.reasonHasNot)
		}
		if ok && reason != "" {
			t.Errorf("%s: a reason for something that worked: %q", c.name, reason)
		}
	}
	// A reason is one line, and a long one is cut.
	if got := lastSaying("a\n" + strings.Repeat("x", 1000)); len(got) > 310 || strings.Contains(got, "\n") {
		t.Errorf("a long line came back %d bytes long", len(got))
	}
	if got := lastSaying("   -\r   \\\r   |\r   /\r"); got != "" {
		t.Errorf("a spinner was taken for words: %q", got)
	}
}

func TestOnlyTheEndOfALongOutputIsKept(t *testing.T) {
	var b tailBuffer
	for i := range 20000 {
		fmt.Fprintf(&b, "line %05d %s\n", i, strings.Repeat("x", 40))
	}
	s := b.String()
	if len(s) > tailKeep {
		t.Errorf("kept %d bytes, want at most %d", len(s), tailKeep)
	}
	if !strings.HasSuffix(s, fmt.Sprintf("line %05d %s\n", 19999, strings.Repeat("x", 40))) {
		t.Errorf("the end of the output is not what was kept: %q", s[max(0, len(s)-80):])
	}
	b.Write([]byte("last"))
	if !strings.HasSuffix(b.String(), "last") {
		t.Error("a write after that was lost")
	}
}

// ---------- reading the installed version ----------

func TestTheInstalledVersionIsReadFromYTDLPAndRememberedWhileTheFileIsTheSame(t *testing.T) {
	rig := newUpdateRig(t, ytInstStandalone, "2026.06.01", "")
	var tool ytTool
	ctx := context.Background()

	v, err := tool.installedVersion(ctx, rig.bin, false)
	if err != nil || v != "2026.06.01" {
		t.Fatalf("version = %q, %v; want the one yt-dlp says, not the warning beside it", v, err)
	}
	if got := rig.calls("ytool"); len(got) != 1 || got[0] != "--ignore-config --version" {
		t.Fatalf("yt-dlp was run as %q, want one run asking its version and ignoring configuration", got)
	}
	tool.installedVersion(ctx, rig.bin, false)
	if n := len(rig.calls("ytool")); n != 1 {
		t.Errorf("yt-dlp was run %d times to read a version it had just said", n)
	}
	// Forced, or the file changed: read again.
	tool.installedVersion(ctx, rig.bin, true)
	if n := len(rig.calls("ytool")); n != 2 {
		t.Errorf("a forced look ran yt-dlp %d times in all, want 2", n)
	}
	os.WriteFile(rig.versionFile, []byte("2026.07.01\n"), 0o600)
	later := time.Now().Add(time.Hour)
	os.Chtimes(rig.bin, later, later)
	if v, _ := tool.installedVersion(ctx, rig.bin, false); v != "2026.07.01" {
		t.Errorf("after the file changed the version is still %q", v)
	}
}

func TestAYTDLPThatCannotSayItsVersionIsSaidNotToHaveOne(t *testing.T) {
	rig := newUpdateRig(t, ytInstStandalone, "", "") // no version: the stand-in fails
	var tool ytTool
	if v, err := tool.installedVersion(context.Background(), rig.bin, false); err == nil || v != "" {
		t.Fatalf("version = %q, %v; want an error", v, err)
	}
	st := tool.status()
	if st.Version != "" || !strings.Contains(st.VersionError, "yt-dlp --version failed") {
		t.Errorf("status = version %q, error %q", st.Version, st.VersionError)
	}
	if st.Outdated != nil {
		t.Errorf("outdated = %v for a version nobody could read, want null", *st.Outdated)
	}

	// A yt-dlp that is broken says so in words of its own, and they are not a version.
	os.WriteFile(rig.versionFile, []byte("crash\n"), 0o600)
	if v, err := tool.installedVersion(context.Background(), rig.bin, true); err == nil || v != "" ||
		!strings.Contains(err.Error(), "ModuleNotFoundError") {
		t.Errorf("version = %q, %v; want the complaint as the reason and no version", v, err)
	}

	// Something that is no date is the version, and cannot be compared.
	os.WriteFile(rig.versionFile, []byte("custom-build-7\n"), 0o600)
	v, err := tool.installedVersion(context.Background(), rig.bin, true)
	if err != nil || v != "custom-build-7" {
		t.Errorf("version = %q, %v", v, err)
	}
}

// The warnings go down the same pipe as the version, before it or after it.
func TestTheVersionIsPickedOutOfWhatYTDLPPrints(t *testing.T) {
	for _, c := range []struct {
		lines   []string
		lenient bool
		want    string
		found   bool
	}{
		{[]string{"2026.08.19"}, true, "2026.08.19", true},
		{[]string{"WARNING: this version is older than 90 days", "2026.08.19"}, true, "2026.08.19", true},
		{[]string{"2026.08.19", "WARNING: late"}, true, "2026.08.19", true},
		// A date is a date from a yt-dlp that failed, too; a line that is not one is not.
		{[]string{"2026.08.19"}, false, "2026.08.19", true},
		{[]string{"custom-7", "WARNING: late"}, true, "custom-7", true},
		{[]string{"WARNING: early", "custom-7"}, true, "custom-7", true},
		// What a version is printed after is noise.
		{[]string{"loading plugins", "custom-7"}, true, "custom-7", true},
		{[]string{"ModuleNotFoundError: No module named 'yt_dlp'"}, false, "", false},
		{[]string{"WARNING: only a complaint", "ERROR: and another"}, true, "", false},
		{nil, true, "", false},
		{[]string{strings.Repeat("x", 200)}, true, strings.Repeat("x", 60), true},
	} {
		got, found := versionIn(c.lines, c.lenient)
		if got != c.want || found != c.found {
			t.Errorf("%q (lenient %v): %q, %v; want %q, %v", c.lines, c.lenient, got, found, c.want, c.found)
		}
	}
}

func TestTheVersionIsNotReadWhileYTDLPIsBeingReplaced(t *testing.T) {
	rig := newUpdateRig(t, ytInstStandalone, "2026.06.01", "")
	if err := ytdlpGate.beginUpdate(); err != nil {
		t.Fatal(err)
	}
	defer ytdlpGate.endUpdate()
	var tool ytTool
	if _, err := tool.installedVersion(context.Background(), rig.bin, false); !errors.Is(err, errYTDLPUpdating) {
		t.Errorf("err = %v, want the file to be left alone", err)
	}
	if n := len(rig.calls("ytool")); n != 0 {
		t.Errorf("yt-dlp was run %d times while it was being replaced", n)
	}
}

// ---------- updating ----------

func TestAWingetCopyIsUpdatedByWingetAndSoIsItsDeno(t *testing.T) {
	rig := newUpdateRig(t, ytInstWinget, "2026.06.01", "")
	rig.withWinget(map[string]wingetStep{
		"yt-dlp.yt-dlp": {To: "2026.08.19", Out: "Found yt-dlp [yt-dlp.yt-dlp]\r  ██▒▒  50%\rSuccessfully installed\n"},
		"DenoLand.Deno": {Out: "Successfully installed\n"},
	})
	rig.withDeno()
	var tool ytTool
	st := updateAndWait(t, &tool)

	res := st.Result
	if !res.OK || res.From != "2026.06.01" || res.To != "2026.08.19" || !res.Changed || res.Deno != "updated" || res.Error != "" {
		t.Errorf("result = %+v", *res)
	}
	got := rig.calls("winget")
	if len(got) != 2 || got[0] != wingetYTDLPCall || got[1] != wingetDenoCall {
		t.Errorf("winget was asked\n%q\nwant\n%q then %q", got, wingetYTDLPCall, wingetDenoCall)
	}
	for _, c := range rig.calls("ytool") {
		if strings.Contains(c, "-U") {
			t.Errorf("yt-dlp was asked to update itself as well: %q", c)
		}
	}
	if st.Version != "2026.08.19" {
		t.Errorf("the status shows version %q after the update", st.Version)
	}
	// The gate is open again.
	if err := ytdlpGate.tryEnter(); err != nil {
		t.Errorf("the gate stayed shut after the update: %v", err)
	}
}

func TestADenoThatCameFromElsewhereIsLeftAlone(t *testing.T) {
	rig := newUpdateRig(t, ytInstWinget, "2026.06.01", "")
	rig.withWinget(map[string]wingetStep{"yt-dlp.yt-dlp": {To: "2026.08.19"}})
	replace(t, &lookupTool, func(string) (string, error) { return `C:\tools\deno\deno.exe`, nil })
	var tool ytTool
	st := updateAndWait(t, &tool)
	if got := rig.calls("winget"); len(got) != 1 {
		t.Errorf("winget was asked %q, want only yt-dlp", got)
	}
	if st.Result.Deno != "" {
		t.Errorf("deno = %q for one winget did not install", st.Result.Deno)
	}
}

func TestADenoThatWillNotUpdateDoesNotUndoTheUpdate(t *testing.T) {
	rig := newUpdateRig(t, ytInstWinget, "2026.06.01", "")
	rig.withWinget(map[string]wingetStep{
		"yt-dlp.yt-dlp": {To: "2026.08.19"},
		"DenoLand.Deno": {Exit: 1, Out: "Installer failed with exit code 1603\n"},
	})
	rig.withDeno()
	var tool ytTool
	res := updateAndWait(t, &tool).Result
	if !res.OK || !res.Changed || res.Deno != "failed" || !strings.Contains(res.DenoError, "1603") {
		t.Errorf("result = %+v", *res)
	}
}

func TestAWingetThatFailsIsReportedAndDenoIsNotTried(t *testing.T) {
	rig := newUpdateRig(t, ytInstWinget, "2026.06.01", "")
	rig.withWinget(map[string]wingetStep{
		"yt-dlp.yt-dlp": {Exit: 1, Out: "Downloading\r  ██▒▒  50%\rInstaller hash does not match\n"},
		"DenoLand.Deno": {},
	})
	rig.withDeno()
	var tool ytTool
	res := updateAndWait(t, &tool).Result
	if res.OK || !strings.Contains(res.Error, "Installer hash does not match") {
		t.Errorf("result = %+v, want a failure with winget's reason", *res)
	}
	if got := rig.calls("winget"); len(got) != 1 {
		t.Errorf("winget was asked %q after yt-dlp failed", got)
	}
	if rig.version() != "2026.06.01" {
		t.Error("the version changed in a run that failed")
	}
}

// winget's own report that there is nothing newer is an answer and no failure.
func TestNothingNewerInWingetIsNotAFailure(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("only Windows can end a program with a code as large as winget's")
	}
	rig := newUpdateRig(t, ytInstWinget, "2026.08.19", "")
	rig.withWinget(map[string]wingetStep{"yt-dlp.yt-dlp": {Exit: 0x8A15002B, Out: "No available upgrade found.\n"}})
	var tool ytTool
	tool.latest, tool.latestAt = "2026.09.15", time.Now() // out on GitHub; winget has not got it
	res := updateAndWait(t, &tool).Result
	if !res.OK || res.Changed || res.To != "2026.08.19" {
		t.Errorf("result = %+v", *res)
	}
	if !res.Behind {
		t.Error("the page is not told that a newer release is out than winget offers")
	}
}

func TestAnUpdateThatReachesTheNewestIsNotBehind(t *testing.T) {
	rig := newUpdateRig(t, ytInstWinget, "2026.06.01", "")
	rig.withWinget(map[string]wingetStep{"yt-dlp.yt-dlp": {To: "2026.09.15"}})
	var tool ytTool
	tool.latest, tool.latestAt = "2026.09.15", time.Now()
	if res := updateAndWait(t, &tool).Result; res.Behind || !res.Changed {
		t.Errorf("result = %+v", *res)
	}
}

func TestAStandaloneCopyUpdatesItselfWithDashU(t *testing.T) {
	rig := newUpdateRig(t, ytInstStandalone, "2026.06.01", "")
	t.Setenv(stubUpdateTo, "2026.08.19")
	rig.withWinget(nil) // there is a winget, and it is not for this
	var tool ytTool
	res := updateAndWait(t, &tool).Result
	if !res.OK || res.From != "2026.06.01" || res.To != "2026.08.19" || !res.Changed {
		t.Errorf("result = %+v", *res)
	}
	updates := 0
	for _, c := range rig.calls("ytool") {
		if strings.HasSuffix(c, " -U") {
			updates++
			// The user's own configuration is the only one yt-dlp reads, as for a download.
			if !strings.HasPrefix(c, "--ignore-config") {
				t.Errorf("the update was run as %q", c)
			}
		}
	}
	if updates != 1 {
		t.Errorf("yt-dlp -U was run %d times, want once", updates)
	}
	if n := len(rig.calls("winget")); n != 0 {
		t.Errorf("winget was run %d times for a copy it did not install", n)
	}
}

func TestAFailedDashUIsReportedWithYTDLPsReason(t *testing.T) {
	rig := newUpdateRig(t, ytInstStandalone, "2026.06.01", "")
	t.Setenv(stubUpdateTo, "") // the stand-in fails
	var tool ytTool
	res := updateAndWait(t, &tool).Result
	if res.OK || !strings.Contains(res.Error, "Permission denied") || strings.HasPrefix(res.Error, "ERROR:") {
		t.Errorf("result = %+v, want yt-dlp's reason without its prefix", *res)
	}
	if rig.version() != "2026.06.01" {
		t.Error("the version changed")
	}
}

// A copy that a package manager or pip owns is not touched: godm says how.
func TestACopyAnotherManagerOwnsIsNotUpdatedByGodm(t *testing.T) {
	for _, c := range []struct {
		placed, kind, cmd string
	}{
		{ytInstPip, ytInstPackage, ytPipCommand}, // a launcher in a Scripts folder
		{ytInstScoop, ytInstScoop, "scoop update yt-dlp"},
	} {
		t.Run(c.kind, func(t *testing.T) {
			rig := newUpdateRig(t, c.placed, "2026.06.01", "")
			rig.withWinget(map[string]wingetStep{"yt-dlp.yt-dlp": {To: "2026.08.19"}})
			t.Setenv(stubUpdateTo, "2026.08.19")
			var tool ytTool
			st := tool.status()
			if st.CanUpdate || st.Install != c.kind || st.Command != c.cmd {
				t.Errorf("status = can update %v, install %q, command %q", st.CanUpdate, st.Install, st.Command)
			}
			err := tool.startUpdate()
			var ref *ytRefusal
			if !errors.As(err, &ref) || ref.Reason != "manual" || !strings.Contains(ref.Msg, c.cmd) {
				t.Fatalf("err = %v, want a refusal that names %q", err, c.cmd)
			}
			if rig.version() != "2026.06.01" || len(rig.calls("winget")) != 0 {
				t.Error("something was run for a copy godm does not update")
			}
			for _, call := range rig.calls("ytool") {
				if strings.Contains(call, "-U") {
					t.Errorf("yt-dlp was asked to update itself: %q", call)
				}
			}
			if tool.status().Updating {
				t.Error("it says it is updating")
			}
		})
	}
}

func TestNoUpdateWithoutYTDLPOrWithoutWinget(t *testing.T) {
	t.Run("not installed", func(t *testing.T) {
		t.Setenv("GODM_YTDLP", filepath.Join(t.TempDir(), "nothing"))
		t.Setenv("PATH", t.TempDir())
		var tool ytTool
		var ref *ytRefusal
		if err := tool.startUpdate(); !errors.As(err, &ref) || ref.Reason != "missing" {
			t.Errorf("err = %v, want a refusal because yt-dlp is not there", err)
		}
		if st := tool.status(); st.Found || st.CanUpdate {
			t.Errorf("status = %+v", st)
		}
	})
	t.Run("no winget", func(t *testing.T) {
		newUpdateRig(t, ytInstWinget, "2026.06.01", "") // findWinget finds none
		var tool ytTool
		var ref *ytRefusal
		if err := tool.startUpdate(); !errors.As(err, &ref) || ref.Reason != "winget" {
			t.Errorf("err = %v, want a refusal because winget is not there", err)
		}
		if tool.status().Updating {
			t.Error("it says it is updating")
		}
	})
}

// ---------- not while it is in use ----------

func TestNoUpdateWhileYTDLPIsInUse(t *testing.T) {
	rig := newUpdateRig(t, ytInstStandalone, "2026.06.01", "")
	t.Setenv(stubUpdateTo, "2026.08.19")
	var tool ytTool

	// A download, or an inspection, or a version being read.
	if err := ytdlpGate.tryEnter(); err != nil {
		t.Fatal(err)
	}
	if !tool.status().Busy {
		t.Error("the status does not say yt-dlp is in use")
	}
	var ref *ytRefusal
	if err := tool.startUpdate(); !errors.As(err, &ref) || ref.Reason != "busy" {
		t.Fatalf("err = %v, want it refused because yt-dlp is in use", err)
	}
	for _, call := range rig.calls("ytool") {
		if strings.Contains(call, "-U") {
			t.Fatalf("yt-dlp was updated under a download: %q", call)
		}
	}
	ytdlpGate.leave()
	if tool.status().Busy {
		t.Error("the status says yt-dlp is in use when it is not")
	}
	if res := updateAndWait(t, &tool).Result; !res.OK {
		t.Errorf("result = %+v", *res)
	}
}

// The real thing that holds the gate: a download that is running.
func TestARunningYTDLPDownloadHoldsOffTheUpdate(t *testing.T) {
	rig := newUpdateRig(t, ytInstStandalone, "2026.06.01", "idler")
	beatFile(t)
	t.Setenv(stubUpdateTo, "2026.08.19")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		RunYTDLP(ctx, YTDLPOptions{URL: "https://example.test/watch?v=1", OutDir: t.TempDir(), Binary: "stub"})
	}()
	waitFor(t, 5*time.Second, "the download to be using yt-dlp", func() bool { return ytdlpGate.inUse() > 0 })

	var tool ytTool
	var ref *ytRefusal
	if err := tool.startUpdate(); !errors.As(err, &ref) || ref.Reason != "busy" {
		t.Fatalf("err = %v, want a refusal while a download runs", err)
	}
	cancel()
	<-ended
	if n := ytdlpGate.inUse(); n != 0 {
		t.Fatalf("%d users of yt-dlp are left after the download was stopped", n)
	}
	if res := updateAndWait(t, &tool).Result; !res.OK || rig.version() != "2026.08.19" {
		t.Errorf("result = %+v, version %s", *res, rig.version())
	}
}

func TestAnInspectionHoldsOffTheUpdateToo(t *testing.T) {
	newUpdateRig(t, ytInstStandalone, "2026.06.01", "idler")
	beatFile(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		InspectYTDLP(ctx, YTDLPOptions{URL: "https://example.test/watch?v=1"})
	}()
	waitFor(t, 5*time.Second, "the inspection to be using yt-dlp", func() bool { return ytdlpGate.inUse() > 0 })
	var tool ytTool
	var ref *ytRefusal
	if err := tool.startUpdate(); !errors.As(err, &ref) || ref.Reason != "busy" {
		t.Fatalf("err = %v, want a refusal while a page is being read", err)
	}
	cancel()
	<-ended
	if n := ytdlpGate.inUse(); n != 0 {
		t.Errorf("%d users of yt-dlp are left", n)
	}
}

func TestOnlyOneUpdateAtATime(t *testing.T) {
	newUpdateRig(t, ytInstStandalone, "2026.06.01", "")
	// The gate is held as an update holds it, which is what a second one meets.
	if err := ytdlpGate.beginUpdate(); err != nil {
		t.Fatal(err)
	}
	defer ytdlpGate.endUpdate()
	var tool ytTool
	var ref *ytRefusal
	if err := tool.startUpdate(); !errors.As(err, &ref) || ref.Reason != "updating" {
		t.Errorf("err = %v, want a refusal because one is under way", err)
	}
	if tool.status().Updating {
		t.Error("the refused update says it is updating")
	}
}

// A download that comes while yt-dlp is being replaced waits for it, and an
// inspection, which someone is watching, is told.
func TestADownloadWaitsForAnUpdateAndAnInspectionIsTold(t *testing.T) {
	stubYTDLP(t, "ok")
	replace(t, &ytdlpGate, &ytGate{})
	if err := ytdlpGate.beginUpdate(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GODM_YTDLP", os.Args[0])

	started := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		_, err := RunYTDLP(context.Background(), YTDLPOptions{
			URL: "https://example.test/watch?v=1", OutDir: t.TempDir(), Binary: "stub",
			OnStart: func(YTStart) { started <- struct{}{} },
		})
		done <- err
	}()
	select {
	case <-started:
		t.Fatal("yt-dlp was started while it was being replaced")
	case <-time.After(400 * time.Millisecond):
	}

	began := time.Now()
	if _, err := InspectYTDLP(context.Background(), YTDLPOptions{URL: "https://example.test/watch?v=1"}); !errors.Is(err, errYTDLPUpdating) {
		t.Errorf("inspection err = %v, want it told yt-dlp is being updated", err)
	}
	if time.Since(began) > 2*time.Second {
		t.Error("the inspection waited for the update instead of being told")
	}

	ytdlpGate.endUpdate()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the download that waited failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the download did not go on once the update was over")
	}
}

func TestAPausedDownloadStopsWaitingForAnUpdate(t *testing.T) {
	stubYTDLP(t, "ok")
	replace(t, &ytdlpGate, &ytGate{})
	ytdlpGate.beginUpdate()
	defer ytdlpGate.endUpdate()
	t.Setenv("GODM_YTDLP", os.Args[0])
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := RunYTDLP(ctx, YTDLPOptions{URL: "https://example.test/watch?v=1", OutDir: t.TempDir(), Binary: "stub"})
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want the pause", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a paused download went on waiting for the update")
	}
}

// ---------- looking, and what a failed download says ----------

func TestACheckThatIsAskedForClearsTheLastResultAndMakesNoClaimWhileUpdating(t *testing.T) {
	rig := newUpdateRig(t, ytInstStandalone, "2026.06.01", "")
	t.Setenv(stubUpdateTo, "2026.08.19")
	var gh fakeGitHub
	gh.set("2026.08.19", nil)
	gh.install(t)
	var tool ytTool
	updateAndWait(t, &tool)
	if tool.status().Result == nil {
		t.Fatal("no result to clear")
	}
	// The dialog being opened looks, and must leave how the update went to be
	// read: else a person who came back to see what became of it would be shown a
	// newer release available, and no word of why it was not installed.
	if st := tool.check(context.Background(), false); st.Result == nil {
		t.Error("a look that was only the dialog opening forgot how the update went")
	}
	st := tool.check(context.Background(), true)
	if st.Result != nil {
		t.Error("the result of the last update stayed after a look that was asked for")
	}
	if st.Outdated == nil || *st.Outdated || st.Version != "2026.08.19" || st.Latest != "2026.08.19" {
		t.Errorf("status = %+v", st)
	}

	// While an update is under way nothing is read from the file.
	ytdlpGate.beginUpdate()
	tool.mu.Lock()
	tool.updating = true
	tool.mu.Unlock()
	before := len(rig.calls("ytool"))
	tool.check(context.Background(), true)
	if n := len(rig.calls("ytool")); n != before {
		t.Errorf("yt-dlp was run %d times by a check during an update", n-before)
	}
	tool.mu.Lock()
	tool.updating = false
	tool.mu.Unlock()
	ytdlpGate.endUpdate()
}

// failingDownload runs a yt-dlp download that fails in a manager and returns the
// reason the task gives.
func failingDownload(t *testing.T, m *Manager) string {
	t.Helper()
	m.announce = nil
	id, err := m.Add(jobRequest{URL: "https://example.test/watch?v=1", Kind: "yt-dlp", Variant: -1})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the download to fail", stateIs(m, id, StateError))
	v, _ := findTask(m, id)
	return v.Error
}

func TestAFailedDownloadSaysWhenYTDLPIsOlderThanTheNewestRelease(t *testing.T) {
	for _, c := range []struct {
		name    string
		have    string // what yt-dlp says its version is; empty: it cannot
		latest  string
		fetch   error
		wantHas string
	}{
		{"older", "2026.06.01", "2026.09.15", nil, "may be out of date (installed 2026.06.01, newest 2026.09.15)"},
		{"the same", "2026.09.15", "2026.09.15", nil, ""},
		{"newer, a nightly", "2026.10.01", "2026.09.15", nil, ""},
		{"a nightly of the same day", "2026.09.15.123456", "2026.09.15", nil, ""},
		{"GitHub cannot be asked", "2026.06.01", "", errors.New("could not reach GitHub"), ""},
		{"no version to be had", "", "2026.09.15", nil, ""},
		{"a version that is no date", "custom-build", "2026.09.15", nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			newUpdateRig(t, ytInstStandalone, c.have, "unavailable")
			var gh fakeGitHub
			gh.set(c.latest, c.fetch)
			gh.install(t)
			m := NewManager(t.TempDir(), 2)

			got := failingDownload(t, m)
			if !strings.Contains(got, "Video unavailable") && !strings.Contains(got, "yt-dlp failed") {
				t.Fatalf("the reason is %q, want yt-dlp's own", got)
			}
			if c.wantHas == "" {
				if strings.Contains(got, "out of date") {
					t.Errorf("the reason claims yt-dlp is old when that is not known: %q", got)
				}
				return
			}
			if !strings.Contains(got, c.wantHas) {
				t.Errorf("the reason is %q, want it to say %q", got, c.wantHas)
			}
			if !strings.Contains(got, "update it in Settings") {
				t.Errorf("the reason does not say where to update: %q", got)
			}
		})
	}
}

func TestTheNoteThatYTDLPIsOldIsInTheLanguageOfGodm(t *testing.T) {
	newUpdateRig(t, ytInstStandalone, "2026.06.01", "unavailable")
	var gh fakeGitHub
	gh.set("2026.09.15", nil)
	gh.install(t)
	m := NewManager(t.TempDir(), 2)
	setLanguage(t, m, langZH)
	got := failingDownload(t, m)
	if !strings.Contains(got, "可能已过期（已安装 2026.06.01，最新 2026.09.15）") {
		t.Errorf("the reason is %q, want the note in Chinese", got)
	}
}

// What is not yt-dlp failing is not blamed on its age.
func TestAFailureThatIsNotYTDLPsHasNoNoteOfAge(t *testing.T) {
	var gh fakeGitHub
	gh.set("2026.09.15", nil)
	gh.install(t)
	m := NewManager(t.TempDir(), 2)
	for _, err := range []error{
		&YTDLPMissingError{},
		errors.New("could not start yt-dlp: access denied"),
		errors.New("only http and https URLs can be handed to yt-dlp"),
		errYTDLPUpdating,
	} {
		if got := m.ytdlpFailureText(err); got != err.Error() {
			t.Errorf("%v became %q", err, got)
		}
	}
	// Whereas the same copy failing on its own is.
	newUpdateRig(t, ytInstStandalone, "2026.06.01", "")
	if got := m.ytdlpFailureText(&YTDLPFailedError{"yt-dlp failed: boom"}); !strings.Contains(got, "out of date") {
		t.Errorf("a failure of yt-dlp's own got %q", got)
	}
}

func TestAFailedInspectionSaysSoToo(t *testing.T) {
	newUpdateRig(t, ytInstStandalone, "2026.06.01", "unavailable")
	var gh fakeGitHub
	gh.set("2026.09.15", nil)
	gh.install(t)
	s := &server{mgr: NewManager(t.TempDir(), 2), token: "tok"}
	api := httptest.NewServer(s.routes())
	defer api.Close()

	body := apiCall(t, api.URL, "tok", http.MethodPost, "/api/inspect", `{"url":"https://example.test/watch?v=1","kind":"yt-dlp"}`)
	if body["ok"] != false || !strings.Contains(fmt.Sprint(body["error"]), "out of date (installed 2026.06.01, newest 2026.09.15)") {
		t.Errorf("answer = %v", body)
	}
	// Up to date: just the reason.
	os.WriteFile(os.Getenv(stubVersionFile), []byte("2026.09.15\n"), 0o600)
	s.mgr.yt.installedVersion(context.Background(), os.Getenv("GODM_YTDLP"), true)
	body = apiCall(t, api.URL, "tok", http.MethodPost, "/api/inspect", `{"url":"https://example.test/watch?v=1","kind":"yt-dlp"}`)
	if body["ok"] != false || strings.Contains(fmt.Sprint(body["error"]), "out of date") {
		t.Errorf("answer for an up to date yt-dlp = %v", body)
	}
}

// The note is never allowed to hold a failure up for long: it waits for the
// network only as long as ytHintBudget.
func TestTheNoteDoesNotHoldAFailureUpForLong(t *testing.T) {
	newUpdateRig(t, ytInstStandalone, "2026.06.01", "")
	var gh fakeGitHub
	gh.hold = make(chan struct{}) // GitHub does not answer
	gh.install(t)
	release := sync.OnceFunc(func() { close(gh.hold) })
	defer release()
	replace(t, &ytHintBudget, 300*time.Millisecond)
	m := NewManager(t.TempDir(), 2)
	began := time.Now()
	got := m.ytdlpFailureText(&YTDLPFailedError{"yt-dlp failed: boom"})
	if took := time.Since(began); took > 3*time.Second {
		t.Errorf("the note held the failure up for %v", took)
	}
	if got != "yt-dlp failed: boom" {
		t.Errorf("reason = %q, want no note when the newest release is not known", got)
	}
	// The looking went on without it. When GitHub does answer, the next failure
	// has the answer already and is not held up at all.
	gh.set("2026.09.15", nil)
	release()
	waitFor(t, 5*time.Second, "the look that was given up on to go on", func() bool { return m.yt.knownNewest() != "" })
	began = time.Now()
	got = m.ytdlpFailureText(&YTDLPFailedError{"yt-dlp failed: boom"})
	if !strings.Contains(got, "out of date") || time.Since(began) > time.Second {
		t.Errorf("reason = %q after %v", got, time.Since(began))
	}
}

// ---------- the API ----------

// apiCall makes a request of the daemon's API and returns the JSON it answers.
func apiCall(t *testing.T, base, token, method, path, body string) map[string]any {
	t.Helper()
	req, err := http.NewRequest(method, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("%s %s answered %d and not JSON: %v", method, path, resp.StatusCode, err)
	}
	out["_status"] = float64(resp.StatusCode)
	return out
}

func TestTheYTDLPAddressesAreGuardedAndKeepToTheirMethods(t *testing.T) {
	s := &server{mgr: NewManager(t.TempDir(), 2), token: "tok"}
	api := httptest.NewServer(s.routes())
	defer api.Close()
	for _, c := range []struct {
		method, path string
		code         int
	}{
		{"GET", "/api/ytdlp", 200},
		{"POST", "/api/ytdlp", 405},
		{"GET", "/api/ytdlp/check", 405},
		{"GET", "/api/ytdlp/update", 405},
	} {
		req, _ := http.NewRequest(c.method, api.URL+c.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("%s %s without the token answered %d, want 401", c.method, c.path, resp.StatusCode)
		}
		req.Header.Set("Authorization", "Bearer tok")
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.code {
			t.Errorf("%s %s answered %d, want %d", c.method, c.path, resp.StatusCode, c.code)
		}
	}
}

func TestTheStatusAndACheckOverTheAPI(t *testing.T) {
	newUpdateRig(t, ytInstStandalone, "2026.06.01", "")
	var gh fakeGitHub
	gh.set("2026.09.15", nil)
	gh.install(t)
	s := &server{mgr: NewManager(t.TempDir(), 2), token: "tok"}
	api := httptest.NewServer(s.routes())
	defer api.Close()

	// What is known without asking anyone: where it is and how it got there.
	got := apiCall(t, api.URL, "tok", "GET", "/api/ytdlp", "")
	if got["found"] != true || got["install"] != ytInstStandalone || got["can_update"] != true || got["busy"] != false || got["updating"] != false {
		t.Errorf("status = %v", got)
	}
	if _, has := got["version"]; has || got["outdated"] != nil {
		t.Errorf("a version or a verdict before anybody looked: %v", got)
	}
	if gh.calls.Load() != 0 {
		t.Error("GitHub was asked by a request that only reads the status")
	}

	got = apiCall(t, api.URL, "tok", "POST", "/api/ytdlp/check", "")
	if got["version"] != "2026.06.01" || got["latest"] != "2026.09.15" || got["outdated"] != true {
		t.Errorf("check = %v", got)
	}
	if _, has := got["latest_error"]; has {
		t.Errorf("an error beside a good answer: %v", got)
	}
	// Looked at again within the hour, nobody is asked, unless force says so.
	apiCall(t, api.URL, "tok", "POST", "/api/ytdlp/check", "")
	if gh.calls.Load() != 1 {
		t.Errorf("GitHub was asked %d times by two checks", gh.calls.Load())
	}
	apiCall(t, api.URL, "tok", "POST", "/api/ytdlp/check?force=1", "")
	if gh.calls.Load() != 2 {
		t.Errorf("GitHub was asked %d times after a forced check, want 2", gh.calls.Load())
	}

	// When GitHub cannot be reached the check says so, and not that all is well.
	gh.set("", errors.New("could not reach GitHub"))
	got = apiCall(t, api.URL, "tok", "POST", "/api/ytdlp/check?force=1", "")
	if got["latest_error"] != "could not reach GitHub" {
		t.Errorf("check = %v", got)
	}
}

func TestAnUpdateOverTheAPI(t *testing.T) {
	rig := newUpdateRig(t, ytInstWinget, "2026.06.01", "")
	rig.withWinget(map[string]wingetStep{"yt-dlp.yt-dlp": {To: "2026.08.19"}})
	s := &server{mgr: NewManager(t.TempDir(), 2), token: "tok"}
	api := httptest.NewServer(s.routes())
	defer api.Close()

	got := apiCall(t, api.URL, "tok", "POST", "/api/ytdlp/update", "")
	if got["ok"] != true || got["install"] != ytInstWinget {
		t.Fatalf("update = %v", got)
	}
	waitFor(t, 20*time.Second, "the update to end", func() bool {
		return apiCall(t, api.URL, "tok", "GET", "/api/ytdlp", "")["updating"] == false
	})
	got = apiCall(t, api.URL, "tok", "GET", "/api/ytdlp", "")
	res, _ := got["result"].(map[string]any)
	if res["ok"] != true || res["to"] != "2026.08.19" || res["changed"] != true || got["version"] != "2026.08.19" {
		t.Errorf("status after the update = %v", got)
	}
}

func TestARefusedUpdateOverTheAPIGivesAReasonThePageCanWord(t *testing.T) {
	newUpdateRig(t, ytInstPip, "2026.06.01", "")
	s := &server{mgr: NewManager(t.TempDir(), 2), token: "tok"}
	api := httptest.NewServer(s.routes())
	defer api.Close()

	got := apiCall(t, api.URL, "tok", "POST", "/api/ytdlp/update", "")
	if got["ok"] != false || got["reason"] != "manual" || !strings.Contains(fmt.Sprint(got["error"]), "pip install") {
		t.Errorf("update = %v", got)
	}
	st := apiCall(t, api.URL, "tok", "GET", "/api/ytdlp", "")
	if st["install"] != ytInstPackage || st["can_update"] != false {
		t.Errorf("status = %v", st)
	}
	if !strings.Contains(fmt.Sprint(st["command"]), "pip install") {
		t.Errorf("the status gives no command to run: %v", st)
	}
}
